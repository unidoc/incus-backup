# Traffic control

The server can limit how fast hosts upload backups and download restores over the agent API (`:8007`).
A typical use is to keep nightly backups at full speed but stop them from filling the office uplink
during working hours.

## Rules

A rule (`traffic_rules`, managed at **Operations → Traffic control** or `/api/v1/traffic/rules`) has:

| field | meaning |
|---|---|
| `name` | unique; letters, digits, `.`, `_`, `-` |
| `enabled` | disabled rules are kept but do nothing |
| `direction` | `upload` (host → server: backups) or `download` (server → host: restores) |
| `scope` | `host`: **every** matching host gets its own bucket of `rate_bps`. `global`: **all** matching hosts share one bucket of `rate_bps` |
| `hosts` | comma-separated globs (`path.Match`) on the host name, e.g. `web-*, db-1`. A glob containing `/` matches `repository/host`, e.g. `offsite/*` |
| `rate_bps` | bytes per second, at least 65 536 (64 KiB/s). A slower limit could keep an 80 MiB blob from finishing within the hosts' 30-minute request timeout |
| `burst_bytes` | bucket size; `0` = one second of `rate_bps` (at least 32 KiB) |
| `schedule` | when the rule applies; empty = always (grammar below) |
| `timezone` | IANA zone of the schedule (default `UTC`) |
| `comment` | free text |

### Schedule grammar

Weekly windows separated by `;`. Each window is `DAYS HH:MM-HH:MM`, `DAYS` alone (whole days), or
`HH:MM-HH:MM` alone (every day):

```
mon-fri 08:00-18:00                       weekdays, 8 to 18 local time
sat,sun                                   whole weekend days
22:00-06:00                               every night; a window may cross midnight
fri 22:00-06:00                           Friday night into Saturday morning (Saturday 03:00 matches, Friday 03:00 does not)
mon-fri 08:00-12:00; mon-fri 13:00-18:00  two windows
fri-mon                                   ranges wrap around the week
daily 00:00-24:00                         24:00 ends at midnight
```

Days are `mon tue wed thu fri sat sun`, ranges, lists, or `*`/`daily`. Times are wall-clock times in the
rule's zone. On a DST change the wall clock decides: a window of 02:00-03:00 is empty on the day the
clock jumps from 02:00 to 03:00.

## How rules apply

- **Where.** The agent API passes chunk uploads (`PUT …/chunks/{id}`), index uploads, chunk downloads
  (`GET /v1/chunks/{id}`) and index downloads through the limiter (`internal/server/httpapi/traffic.go`).
  Small JSON calls are not limited.
- **Strictest wins.** A transfer waits for **every** rule that matches its host, direction and current
  time. With a shared 100 MiB/s for all hosts and 10 MiB/s for `db-*`, a `db-` host gets at most
  10 MiB/s. The rest of the hosts share 100 MiB/s, and the `db-` host's traffic counts against the shared
  bucket too.
- **Token buckets.** A bucket refills at `rate_bps` up to `burst_bytes`. Every 32 KiB piece of a body
  reserves its bytes and waits until the bucket can pay for them. Reservations queue up in arrival order,
  so hosts that share a bucket get fair shares, piece by piece. Waits honour the request context: a host
  that disconnects stops waiting at once. No goroutine is started per transfer.
- **Rule changes.** Rules are cached and re-read at least every 30 seconds. A change made through the API
  takes effect at once in this server process. A transfer keeps the buckets it started with, and the next
  chunk request gets the new ones.
- **Database trouble.** If the rules cannot be read, the previous rules stay in force. If none were ever
  loaded, nothing is limited. Traffic shaping never stops a backup.

## Live throughput

`GET /api/v1/traffic/current` returns, for every host seen since the server started:

- the upload and download rate (average of the last 5 complete seconds);
- the bytes uploaded and downloaded since start;
- the time of the last transfer.

It also lists the rules in force and whether each is inside its window now. The Traffic control page
polls it every 2 seconds. The counters are in memory and start at zero when the server restarts.

## Measured (tests)

`internal/server/traffic/traffic_test.go` uses real time on a loaded shared machine, with a tolerance of
±20 %:

| case | expected | measured |
|---|---|---|
| 3 MiB upload at 1 MiB/s, 64 KiB burst | 2.94 s | 2.94 s |
| 5 MiB download at 2 MiB/s | 2.47 s | 2.47 s |
| live rate during a 1 MiB/s upload | 1 048 576 B/s | 1 048 576 B/s |
| two hosts, `host` scope 1 MiB/s, 3 MiB each, in parallel | 2.94 s each | 2.94 s, 2.94 s |
| two hosts sharing a `global` 2 MiB/s, 3 MiB each | 2.97 s each | 2.94 s, 2.97 s |
| 8 MiB/s global plus 1 MiB/s for the host, 2 MiB | 1.94 s | 1.94 s |
| after raising the host rule to 4 MiB/s, 4 MiB | 0.98 s | 0.99 s |
| Wednesday 10:00 inside `mon-fri 08:00-18:00`, 2 MiB at 1 MiB/s | 1.94 s | 1.94 s |
| Sunday, outside the window, 8 MiB | unlimited | < 0.5 s |
| 20 transfers cancelled after 300 ms | stop at once, no goroutine left | passed (also under `-race`) |

`internal/server/mgmt/routes_traffic_test.go` runs end to end: a rule created through the management API
throttles a real host's 1 MiB backup over mutual TLS at 256 KiB/s. That took 3.80 s, against 3.75 s
expected. `/traffic/current` then reports the host and its bytes. After the rule is deleted, the next
backup is not throttled.
