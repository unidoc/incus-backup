# Validation gates (real Incus, real ZFS, scratch objects only)

The sandbox cannot run Incus. These scripts are how the open items of
`docs/design.md` are closed on a real host. They are written to be safe:

* everything is created inside the Incus project **`ibgate`** (marked `user.ibgate=1`);
* a gate **refuses to run** if that project already exists;
* cleanup deletes only that project and `/var/tmp/ibgate.*`;
* nothing in any other project (e.g. `custody`) is read, written or deleted.

| Script | Closes | Needs |
|---|---|---|
| `g-e2e.sh` | G2 (VM backup/restore, real NBD + bitmaps + client + server), G3 (container snapshot read path, restore), deep verify | the built `incus-backup` binary, a reachable server with a **scratch repository** and a token for it |
| `g-freeze.sh` | G2a (cost of a long frozen view; with `QUOTA_TEST=1` what the guest sees when the overlay cannot grow) | `zfs`, `nbdpoc` (`temp/nbdpoc/`), a ZFS pool |

Run on the Incus host (Alpine: busybox `sh` is enough):

```sh
# server (Cikarang): a scratch repository, never the production one
incus-backup-server repo create ibgate --datastore /srv/incus-backup-gate
incus-backup-server -r ibgate token create benfica-gate --scope backup

# host (Benfica)
IBIN=/usr/bin/incus-backup SERVER=backup.example.net:8007 TOKEN=ibt1.... sh g-e2e.sh run
NBDPOC=/root/nbdpoc/nbdpoc-linux-amd64 sh g-freeze.sh run 600
NBDPOC=/root/nbdpoc/nbdpoc-linux-amd64 QUOTA_TEST=1 sh g-freeze.sh run 600
```

`KEEP=1` leaves the objects in place for inspection; `sh <script> cleanup` removes
them. Paste the output back: every `FAIL` line names the assumption in the code
that needs fixing.
