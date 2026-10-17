# Testing against S3

S3 tests need no external service. By default `internal/s3test` serves an in-process S3 server
(`internal/s3test/s3fake`): pure Go, standard library only, one loopback listener. MinIO is not used any
more; its server is no longer distributed or maintained.

The fake is only worth something if it behaves like a real S3 service, so three things pin it down:

1. it is **strict**: it implements exactly the S3 behaviour the repository relies on and answers everything
   else with `501 NotImplemented` (multipart upload, tagging, ACLs, policies, ListObjects V1, presigned URLs).
   A code change that starts using such a call fails loudly;
2. a **conformance suite** (`internal/s3test/conformance`, 36 checks) runs against the fake in every
   `go test` and against any real S3 service on demand;
3. **mutation tests** switch single defects on in the fake (ignore `If-None-Match`, no continuation tokens,
   accept any signature, ...) and require the suite to fail, so neither the fake nor the suite can rot silently.

## What the fake implements

* Path-style addressing; **AWS Signature V4**, recomputed from the specification: unknown access key
  (`InvalidAccessKeyId`), wrong secret, a query, path, method or signed header changed after signing
  (`SignatureDoesNotMatch`), an `x-amz-*` header added after signing, clock skew over 15 minutes
  (`RequestTimeTooSkewed`), wrong region and a missing `x-amz-content-sha256` are refused with the S3 codes.
  Plain-HTTP uploads are `aws-chunked` (what minio-go sends): every chunk signature in the chain is verified,
  the object is committed only after the verified final chunk, so a torn upload leaves nothing.
* Buckets: create (also with object lock), head, location, delete (`BucketNotEmpty`), list.
* Objects: `PutObject` single part with an **atomic** `If-None-Match: *` (`412 PreconditionFailed`),
  `Content-MD5` and `x-amz-checksum-*` verification, user metadata, ETag (MD5), `Last-Modified`; `GetObject`
  with `Range` (`206`, `416 InvalidRange`) and `If-Match` / `If-None-Match`; `HeadObject`; `DeleteObject`
  (idempotent); `DeleteObjects` (multi-delete XML); `CopyObject`.
* `ListObjectsV2`: prefix, delimiter, `max-keys`, continuation tokens, `start-after`, `encoding-type=url`;
  `ListObjectVersions`.
* Versioning (delete markers, version ids), lifecycle and object-lock configuration (stored and answered as S3
  does for a plain bucket and a configured one; retention is **not** enforced).
* Not implemented, by design: multipart upload (the repository sets `DisableMultipart`; a request for it is
  logged and answered `501`), tagging, ACLs, policies, ListObjects V1, presigned URLs, `If-Match` on PUT.

## The Go API

```go
func TestMain(m *testing.M) { s3test.Main(m) }          // or testdb.AtExit(s3test.Start()); testdb.Main(m)

svc, bucket := s3test.NewBucket(t)                      // fresh bucket on the shared service (fake or real)
srv, svc := s3test.NewServer(t)                         // a private fake: fault injection, log, counters
svc, bucket = s3test.NewBucketAt(t, svc)
```

`NewServer` always gives a fake, whatever `INCUS_BACKUP_TEST_S3_*` says, so fault-injection tests run in every
environment (they never skip). On the `*s3test.Server`:

| | |
|---|---|
| `FailNth(op, n, status, code)`, `FailNext(op, times, status, code)` | answer with an S3 error XML (`503 SlowDown`, `500 InternalError` are retried by minio-go, `403 AccessDenied` is not) |
| `SlowDownBurst(n)` | the next `n` requests get `503 SlowDown`. Keep `n` below minio-go's retry budget (10 attempts, 200 ms to 1 s backoff) unless the budget is the point |
| `TearNextUpload(bytes)` | reset the connection after that many body bytes of the next `PutObject`; the object is **not** stored |
| `DropNth(op, n)`, `LoseNextResponse(op)` | reset the connection; or perform the operation and then reset instead of answering (the client retries a request that already took effect) |
| `SetLatency(d)`, `AddRule(Rule{... Action{Delay, Hang}})` | latency for all requests, or per matching request; a hanging request ends with the client's context |
| `Requests()`, `Count(op)`, `HeaderValues(op, h)`, `PeakInflight()`, `ResetLog()` | the request log: method, path, operation, bucket, key, headers, status, bytes, duration, the fault that hit it |
| `Object(bucket, key)`, `Keys(bucket)` | server-side state, for assertions such as "no object was left behind" |
| `AddUser`, `RemoveUser` | credential handling: a rotated or revoked key |

Rules match on operation (`PutObject`, `GetObject`, `HeadObject`, `ListObjectsV2`, `DeleteObject`,
`DeleteObjects`, `CopyObject`, ...), bucket, key prefix or a function; `Skip` and `Times` choose which of the
matching requests are hit. A rule returns a handle with `Hits()`, `Seen()` and `Remove()`; assert on `Hits()`
so a fault that never fired cannot make a test pass.

## Levels

| Command | What it runs | Needs |
|---|---|---|
| `go test ./...` | everything, S3 tests on the in-process fake; the conformance suite against the fake; the mutation tests | PostgreSQL (embedded) for the server tests |
| `just test-s3` | `go test ./internal/s3test/...`: fake, fault injection, conformance, mutation tests | nothing |
| `INCUS_BACKUP_TEST_S3_ENDPOINT=host:port INCUS_BACKUP_TEST_S3_ACCESS_KEY=... INCUS_BACKUP_TEST_S3_SECRET_KEY=... go test ./internal/s3test/conformance ./internal/datastore` | the SAME tests against that real service (`https://host:port` for TLS) | a real S3 service |
| `just test-s3-real` (`scripts/test-s3-real.sh`) | builds versitygw at the pinned version, starts it on a temporary directory, runs the line above | Go, network for the first build |
| CI job `s3-conformance` | the same, with versitygw built from source at a pinned version | |

With `INCUS_BACKUP_TEST_S3_*` set, `TestConformance` runs against that service instead of the fake. A service
that lacks an optional feature declares it with `INCUS_BACKUP_TEST_S3_UNSUPPORTED=lifecycle,versioning,...`
(features: `versioning`, `lifecycle`, `object-lock`, `multi-delete`, `copy`). Nothing is skipped: for a declared
feature the suite requires the service to REFUSE it with an S3 error, so a wrong declaration fails too.

If the endpoint is set but unusable, tests fail. A test that silently skips proves nothing.

## Checked against a real server

The conformance suite and the whole `internal/datastore` package were run against **versitygw v1.9.0**
(Apache-2.0, posix backend, `--versioning-dir`; built with `go install github.com/versity/versitygw/cmd/versitygw@v1.9.0`):

* conformance: 36 of 36 pass with `lifecycle` declared unsupported (versitygw answers bucket lifecycle with
  `501 NotImplemented`; that refusal is itself checked). Passing there includes `If-None-Match: *` under 16
  concurrent writers (exactly one wins), torn uploads leaving no object, listing order, pagination with
  continuation tokens and common prefixes, special-character keys, versioning with delete markers, object
  lock configuration, multi-delete, copy, and every authentication refusal;
* `go test ./internal/datastore`: passes (about 45 s, the same as on the fake).

A side-by-side probe of about 60 raw requests against the fake and versitygw found, and the fake was
corrected for: `max-keys=0` reported `IsTruncated=true`; a `416` answer carried an `ETag`. Differences that are
left, because the repository never depends on them and the fake is meant to be strict like AWS:
versitygw accepts a garbage continuation token or `encoding-type` (AWS and the fake answer `400
InvalidArgument`), answers `400 InvalidArgument` to a malformed `versionId` (the fake `404 NoSuchVersion`),
implements `If-Match` on PUT, ListObjects V1, ACL and tagging (the fake `501`), answers a multi-delete with a
bad body `InvalidRequest` (the fake `MalformedXML`), and a posix backend cannot store an object and a
"directory" under the same name (S3 can; the in-memory backend tests cover that case).

SeaweedFS and Garage were not tried: versitygw worked first.

## When S3 behaviour the code relies on changes

1. Add or extend the behaviour in `s3fake`, strictly (what the code sends, what AWS answers).
2. Add a conformance check in `internal/s3test/conformance/checks.go` (a function that records failures; it is
   run by `Run`, which is also what the mutation tests call).
3. Add a mutation to `TestSuiteFailsAgainstBrokenServers` for the new behaviour if it can silently break.
4. Run `just test-s3-real`. If the real server disagrees, the real server is right: fix the fake and the check.
