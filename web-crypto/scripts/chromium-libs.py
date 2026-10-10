#!/usr/bin/env python3
"""Unpack the shared libraries Playwright's headless Chromium needs into .cache/chromium-libs/,
for Debian containers that lack them and where apt cannot be used (no root). It reads the
Debian package index (default: trixie), resolves the dependency closure of Chromium's library
packages minus what dpkg says is installed, downloads the .deb files and extracts them with
dpkg-deb -x. browser/run.mjs adds the result to the browser's LD_LIBRARY_PATH by itself.

    python3 scripts/chromium-libs.py [--suite trixie] [--mirror https://deb.debian.org/debian]
"""
import argparse
import lzma
import os
import subprocess
import sys
import urllib.request

WANT = [
    'libasound2t64', 'libatk-bridge2.0-0t64', 'libatk1.0-0t64', 'libatspi2.0-0t64', 'libcairo2',
    'libcups2t64', 'libdbus-1-3', 'libexpat1', 'libgbm1', 'libglib2.0-0t64', 'libnspr4', 'libnss3',
    'libpango-1.0-0', 'libudev1', 'libx11-6', 'libxcb1', 'libxcomposite1', 'libxdamage1', 'libxext6',
    'libxfixes3', 'libxkbcommon0', 'libxrandr2',
]
SKIP = {'libc6', 'libgcc-s1', 'libstdc++6', 'debconf', 'dpkg', 'adduser'}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--suite', default='trixie')
    ap.add_argument('--mirror', default='https://deb.debian.org/debian')
    a = ap.parse_args()
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    cache = os.path.join(here, '.cache', 'chromium-libs')
    debs = os.path.join(cache, 'debs')
    root = os.path.join(cache, 'root')
    os.makedirs(debs, exist_ok=True)
    os.makedirs(root, exist_ok=True)
    url = f'{a.mirror}/dists/{a.suite}/main/binary-amd64/Packages.xz'
    print('reading', url)
    text = lzma.decompress(urllib.request.urlopen(url).read()).decode('utf-8', 'replace')
    pk, prov = {}, {}
    for block in text.split('\n\n'):
        cur = {}
        for line in block.split('\n'):
            if line and line[0] not in ' \t' and ': ' in line:
                k, _, v = line.partition(': ')
                cur[k] = v
        if 'Package' in cur:
            pk[cur['Package']] = cur
            for p in cur.get('Provides', '').split(','):
                p = p.strip().split(' ')[0]
                if p:
                    prov.setdefault(p, cur['Package'])
    out = subprocess.run(['dpkg-query', '-W', '-f=${Package}\n'], capture_output=True, text=True).stdout.split()
    installed = {x.split(':')[0] for x in out}
    need, stack = set(), list(WANT)
    while stack:
        n = stack.pop()
        if n in need or n in SKIP:
            continue
        if n not in pk:
            if n not in prov:
                print('not in the index:', n, file=sys.stderr)
                continue
            n = prov[n]
        if n in installed:
            continue
        need.add(n)
        for alt in pk[n].get('Depends', '').split(','):
            d = alt.split('|')[0].strip().split(' ')[0].split(':')[0]
            if d:
                stack.append(d)
    for n in sorted(need):
        fn = pk[n]['Filename']
        dst = os.path.join(debs, os.path.basename(fn))
        if not os.path.exists(dst):
            urllib.request.urlretrieve(f'{a.mirror}/{fn}', dst)
        subprocess.run(['dpkg-deb', '-x', dst, root], check=True)
    print(f'{len(need)} packages unpacked into {root}')


if __name__ == '__main__':
    main()
