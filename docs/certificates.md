# Certificate lifecycle

incus-backup-server runs a small private CA (ECDSA P-256, `internal/pki`). Every
client host and every sync peer authenticates with a certificate signed by it. The
server authenticates to clients with a server certificate from the same CA. Clients
pin the CA by its fingerprint, which comes from the one-time enrolment token.
Hostnames are never checked: the CA is the identity.

The goal of this document is to keep that system **boring**. Nothing expires
silently, and nothing needs a restart or a manual step except a CA rotation once a
decade.

| Certificate | Validity | Renewed | By |
|---|---|---|---|
| CA (`ca.crt`) | 10 years | rotated (below) | operator: `ca rotate` / `ca promote` |
| server certificate | 2 years | < 45 days left, SAN edit, CA promotion | server daemon (checks at start and every minute) |
| client host | 2 years | < 45 days left, or the server's signing CA changed | client daemon (checks at start and daily), `incus-backup cert renew` |
| sync peer credential | 2 years | as client hosts | sync manager, before every run of the job |

## Host certificates are a set

`host_certs(fingerprint, host_id, not_before, not_after, created_at, revoked, ...)`
holds every certificate an identity (host or peer: one row of `hosts`) was issued.
`hosts.cert_fingerprint` still names the **current** one. The server never stores the
certificates themselves; `not_before`/`not_after` are recorded when a certificate is issued and
confirmed the first time it is presented. (Pre-release builds had a migration that backfilled one row
per existing host; the consolidated schema starts empty.)

A request is authenticated (`HostFromCert`) when all of these hold:

- The certificate is signed by a CA the server trusts.
- It is valid at the service clock.
- Its fingerprint is in `host_certs` and not revoked.
- Its identity is not disabled.

Disabling a host therefore rejects every certificate it holds. Re-enabling restores
them, because disabling revokes nothing. Re-enrolling an identity with a new token
revokes all of its earlier certificates (the old behaviour).

The TLS layer only checks the signature: `ClientAuth=RequestClientCert` plus
`pki.VerifyClientCert`, with the time clamped into the certificate's own validity.
Time is judged by the service, so the renewal path can accept a certificate that
expired recently, and tests can drive the clock.

## Renewal protocol

```
client                                   server
  new key + CSR
  POST /v1/renew  (mTLS: old cert) ---->  check: signed by us, not revoked, host enabled,
                                          not expired more than 30 days ago,
                                          current (or the parent of a never-used current),
                                          at most one renewal per identity per 10 min
                                   <----  new cert (same identity/scope, CN = host name),
                                          CA trust bundle; new cert becomes current,
                                          old cert stays valid (overlap)
  write ca.crt, client.crt/.key atomically, keep .prev
  switch to a NEW transport
  GET /v1/cert   (new cert)  ---------->  first use of the new cert is recorded
  POST /v1/renew/confirm (new cert) ---->  every other certificate of the identity is revoked
```

**Overlap.** The old certificate stays valid until it expires or until the client
confirms. Requests already in flight in the same process finish on their old
connection. Another process using the same directory (a `incus-backup run` next to the
daemon) gets a 401 after the confirmation. It then re-reads the credential directory
and retries the request once (`remote.Client.Reload`).

**Lost answers.** If the answer to `/v1/renew` is lost, the client still has only
the old certificate, which is no longer current. The server lets that old certificate
renew once more, provided the current certificate was never used and never
confirmed. The orphan is then revoked. An identity never holds more than two valid
certificates.

**Concurrency.** The host row is locked (`SELECT ... FOR UPDATE`) and the
per-identity interval applies. Of N concurrent renewals with one certificate, exactly
one wins; the others get 429. Clients also serialise renewals of one directory with
`flock(.renew.lock)`. `/v1/renew` is additionally rate limited per address.

**Refusals.** These are the refusals and their status codes:

- A disabled host gets 403.
- A certificate expired more than 30 days ago gets 401, saying "enrol again". This is
  `pki.RenewGrace`; the host needs a new token.
- A certificate that was superseded by a used one gets 409.
- A foreign certificate gets 401.

**Client failure handling.** These are the failure cases and what the client does:

- **The new certificate is refused at `/v1/cert` (401/403).** The client rolls back to
  the previous pair, which is still valid on the server. The next attempt uses the
  lost-answer rule.
- **A network error after the files were written.** The client keeps the new pair and
  sets `confirm_pending`. The daemon retries the confirmation hourly, and the old
  certificate stays valid meanwhile.
- **The renewal request itself fails.** The client backs off for 1 h, 2 h, 4 h, …,
  at most 24 h. The state lives in `renew.json` next to the credentials, and `doctor`,
  `status` and `cert status` show it.

**Crash safety.** Every file is replaced by writing a temp file, fsyncing it,
renaming it and fsyncing the directory. Two renames are not atomic together, so the
loader (`pki.LoadPair`) picks the first matching pair among
{crt, crt.prev} × {key, key.prev}. A crash between the renames therefore still loads
the previous, still valid pair.

## Server certificate: renewal and hot reload

The listener's `tls.Config` uses `GetConfigForClient` (plus `GetCertificate`). For
every handshake it returns the current certificate and trust pool from an atomically
replaced snapshot. The snapshot is re-read from the state directory when any PKI
file's size or mtime changed, checked at most once a second. As a result, a
certificate replaced by any process is served to new connections within about a
second, while open connections keep theirs. `TestProductionListenerHTTP2AndHotReload` runs the real `Server.Serve` and checks
both HTTP/2 and the certificate swap. `GetCertificate` alone would not work:
Go never consults it for clients without SNI (IP addresses) when `Certificates` is
set.

`CertLoop` runs in `serve` and checks at start and every minute. It reissues the
server certificate in these cases:

- It expires within 45 days.
- The SAN list in PostgreSQL (`server_settings.sans`) differs from its SANs.
- It was not issued by the presented CA (after `ca promote`).

Issuing is serialised across processes with a PostgreSQL advisory lock. Without a
`sans` row, the SANs of the current certificate apply: that is the bootstrap value
given to `init --san`.

- `incus-backup-server ca sans` prints the SANs.
- `ca sans NAME|IP ...` replaces them (`service.SetServerSANs`).

## Status

`service.CertificateStatus(ctx)` returns a `CertReport` for the UI and for
notifications:

- **CA:** subject, fingerprint, `not_after`.
- **Rotation:** whether a rotation is under way, with the pending identities.
- **Server certificate:** its SANs and the configured SANs.
- **Each identity:** repository, name, scope, `not_after`, last renewal, last seen, and
  the number of other still-valid certificates.
- **This server's own sync-peer credentials:** read from
  `state_dir/sync/<repo>/<job>/`, with the last renewal error.

Each item has a state (`ok | expiring | expired`; a host whose certificate came from a
CA that is no longer trusted, after `ca promote --force`, is `untrusted`) and a level
(`ok | warn | critical`). Warn is below 60 days and critical below 14 days. For the CA,
warn is below 1 year and critical below 90 days. `Level` is the worst of all items.
Clients renew at 45 days, so a healthy host shows `warn` for the 15 days between 60
and 45 days left, then returns to `ok`. A host that is still `warn` below 45 days, or
reaches `critical`, has a failing renewal. A notification rule that should page only
on real problems can use "below 44 days" as its trigger.

- CLI: `incus-backup-server ca status [--json]`. It exits non-zero on `critical`.
- Client CLI: `incus-backup cert status`.

## CA rotation

**Constraint.** Clients pin a CA. Old clients look for that CA's fingerprint in the
chain the server presents, and cross-signing does not help. The CA is created with
`MaxPathLen=0`, so it cannot certify an intermediate, and every existing deployment
has such a CA. The server therefore keeps presenting the old CA until every client
has the new one in its trust bundle.

**Procedure (implemented and tested).**

1. **`incus-backup-server ca rotate`.** This creates `ca-next.crt/.key`. Both CAs are
   now trusted, and the new one signs every client certificate from now on (enrolment
   and renewal). The server still presents the old CA, and tokens still pin it.
   Renewal and enrolment answers carry the bundle [old, new].
   - Every client and peer sees through `GET /v1/cert` that the signing CA changed.
     It renews early at its next daily check (or at the next sync run), and stores
     the bundle.
   - `ca status` lists the identities still pending: their current certificate is
     not from the new CA, or was never used, which would mean the client may not have
     the bundle.
2. **`incus-backup-server ca promote`.** This is refused while anything is pending;
   `--force` cuts off the pending identities, which must then enrol again.
   - The new CA becomes `ca.crt` and the old one is archived as
     `ca-retired-<fp>.crt/.key`.
   - The server certificate is reissued from the new CA, the old CA is no longer
     trusted, and new tokens pin the new CA.
   - The running daemon picks all of this up within a second, without a restart.

**Timing.** Start the rotation in the CA's last year, which is when `ca status` warns.
Promote after a day or two, when nothing is pending. Disabled identities never block
a promotion.

**Tested.**

- `TestCARotation` (service).
- `TestCARotationOverTLS` (httpapi), over real TLS:
  - A client enrolled before the rotation keeps working.
  - It renews early, stores the bundle, and still trusts the server after promotion.
  - A client that never renewed is cut off.
- `TestCACommands` (CLI).

**Not proven.** Rotation has not been run on a real deployment, with real time and
real daemons on separate machines.

## Files

Server state directory:

| File | Contents |
|---|---|
| `ca.crt`, `ca.key` | the presented CA |
| `ca-next.crt`, `ca-next.key` | only during a rotation |
| `ca-retired-*` | archived CAs |
| `server.crt`, `server.key` | the server certificate |
| `*.prev` | the previous pairs |

Client credential directory (`state_dir/servers/<name>/`), and the same layout for a
sync job under `state_dir/sync/<repo>/<job>/` on the server:

| File | Contents |
|---|---|
| `ca.crt` | the trust bundle |
| `client.crt`, `client.key` | the client certificate |
| `client.*.prev`, `ca.crt.prev` | the previous files |
| `renew.json` | renewal state |
| `.renew.lock` | renewal lock |
