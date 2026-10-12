#!/usr/bin/env python3
"""Independent check of internal/format/testdata/vectors.json.

Re-implements the construction from docs/format.md with PyNaCl and
`cryptography` (NOT the Go code) and verifies every vector. Run:

    python3 -m venv v && v/bin/pip install pynacl cryptography
    v/bin/python scripts/verify-vectors.py
"""
import hashlib, hmac, json, sys, base64
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from nacl.bindings import crypto_aead_xchacha20poly1305_ietf_decrypt as xdec

v = json.load(open(sys.argv[1] if len(sys.argv) > 1 else "internal/format/testdata/vectors.json"))
ok = True
def check(name, cond):
    global ok
    print(("PASS " if cond else "FAIL ") + name)
    ok = ok and cond

master = bytes.fromhex(v["master_hex"])
def derive(info):
    return HKDF(algorithm=hashes.SHA256(), length=32, salt=b"incus-backup/v1", info=info.encode()).derive(master)
k_enc, k_id, k_man, k_dom = derive("chunk-encryption"), derive("chunk-id"), derive("manifest-encryption"), derive("domain-id")
check("k_enc", k_enc.hex() == v["k_enc_hex"])
check("k_id", k_id.hex() == v["k_id_hex"])
check("k_manifest", k_man.hex() == v["k_manifest_hex"])
check("domain id", k_dom[:8].hex() == v["domain_id"])

plain = bytes.fromhex(v["chunk_plaintext_hex"])
cid = hmac.new(k_id, plain, hashlib.sha256).digest()
check("chunk id = HMAC-SHA256(k_id, plaintext)", cid.hex() == v["chunk_id_hex"])

blob = bytes.fromhex(v["chunk_blob_hex"])
check("blob trailer = SHA-256(preceding)", hashlib.sha256(blob[:-32]).digest() == blob[-32:])
check("blob magic/version/flags", blob[:4] == b"IBCH" and blob[4] == 1 and blob[5] == 0 and blob[6:8] == b"\0\0")
nonce, ct = blob[8:32], blob[32:-32]
check("nonce as recorded", nonce.hex() == v["chunk_nonce_hex"])
aad = blob[:8] + cid
check("blob decrypts (XChaCha20-Poly1305, aad=header|id) to plaintext", xdec(ct, aad, nonce, k_enc) == plain)

group = hmac.new(k_id, b"group\x00custody\x00block\x00custody-benfica-bosch/root", hashlib.sha256).hexdigest()[:32]
check("group id", group == v["group_id"])

idx = bytes.fromhex(v["index_file_hex"])
check("index trailer", hashlib.sha256(idx[:-32]).digest() == idx[-32:])
check("index header", idx[:4] == b"IBIX" and idx[4] == 1 and idx[5] == 1)
check("index total size 31, count 2", int.from_bytes(idx[12:20], "big") == 31 and int.from_bytes(idx[20:28], "big") == 2)
check("index entry 0 id = chunk id", idx[28 + 8:28 + 40].hex() == v["chunk_id_hex"])

mb = bytes.fromhex(v["manifest_blob_hex"])
check("manifest trailer", hashlib.sha256(mb[:-32]).digest() == mb[-32:])
snap = json.loads(v["manifest_json"])["snapshot_id"]
maad = b"IBMF" + bytes([1, 0, 0, 0]) + group.encode() + b"\0" + snap.encode()
pt = xdec(mb[32:-32], maad, mb[8:32], k_man)
check("manifest decrypts and equals recorded JSON semantics", json.loads(pt)["snapshot_id"] == snap and json.loads(pt)["object"]["name"] == "custody-benfica-bosch/root")
check("manifest index digest = sha256(index file)", json.loads(pt)["index_digest"] == hashlib.sha256(idx).hexdigest())

rk = v["recovery_key"]
raw = base64.b32decode(rk[len("IBR1-"):].replace("-", "") + "=" * (-len(rk[len("IBR1-"):].replace("-", "")) % 8))
check("recovery key payload = master", raw[:32] == master)
check("recovery checksum", raw[32:] == hashlib.sha256(b"incus-backup recovery key v1" + master).digest()[:4])
sys.exit(0 if ok else 1)
