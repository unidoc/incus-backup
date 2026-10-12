# Retention (prune) semantics

Retention decides which snapshots of one object group a policy keeps. The code is
`internal/retention.Plan`. The web console previews it with a TypeScript port
(`web/src/lib/retention.ts`). The server applies it in `service.Prune`, and the dry run
(`POST /api/v1/prune?dry_run=1`) returns one decision per snapshot with the reason.

## Same decisions as Proxmox Backup Server

The algorithm is the one in PBS `pbs-datastore/src/prune.rs` (`compute_prune_info` and
`mark_selections`):

1. Snapshots are sorted newest first.
2. The rules run in a fixed order: `keep-last`, `keep-hourly`, `keep-daily`, `keep-weekly`,
   `keep-monthly`, `keep-yearly`.
3. Each rule walks the snapshots and keeps the newest snapshot of each of the N most recent
   periods (an hour, a day, an ISO week, a month or a year) that no earlier rule has covered.
4. **Cumulative.** A period that an earlier rule already kept a snapshot for does not use up a
   slot of a later rule. With one backup a day, `keep-last 3` plus `keep-daily 7` keeps 10
   snapshots, not 7. This is PBS behaviour, not a deviation.
5. **Older snapshots of a used period are removed for good.** When a rule keeps the newest
   snapshot of a period, it decides "remove" for the older snapshots of that period it walks
   past. Every later rule skips them. In PBS this is `PruneMark::Remove`.
6. **Protected snapshots** are always kept and never use up a slot.
7. **No rule at all** keeps everything ("retention disabled").

### Example: an ISO week that straddles a month end

One backup a day at 12:00 UTC, from 1 April to 9 July 2026, with the rules `keep-weekly 2`
and `keep-monthly 3`. ISO week 27 runs from Monday 29 June to Sunday 5 July.

| Rule | Keeps | Walks past (removed for good) |
|---|---|---|
| weekly | 9 Jul (W28), 5 Jul (W27) | 8, 7, 6 Jul (W28); 4 Jul .. 29 Jun (W27) |
| monthly | 28 Jun (June), 31 May, 30 Apr | the rest of each month |

July is already covered by the weekly rule, so it does not use a monthly slot. For June, the
monthly rule cannot take 30 June, because the weekly rule walked past it. It takes 28 June.

Releases before this change kept 30 June here. They only skipped walked-past snapshots inside
the same rule. The audit compared that code with PBS over 20 000 random configurations and found
235 different decisions. In 3 of those configurations it kept one snapshot more than PBS.

### Example: cumulative rules

One backup a day, with `keep-last 3` and `keep-daily 7`. `keep-last` keeps the newest 3
snapshots. Their 3 days are then covered, so `keep-daily` keeps the 7 days before them. That
gives 10 snapshots in total.

## What is deliberately different from PBS

The algorithm is the same. The differences are in what goes into it.

| Topic | PBS | incus-backup | Why |
|---|---|---|---|
| Time zone for periods | the server's local time zone | the **policy's** time zone (`timezone`, default `UTC`) | Hosts in several countries can share one server. "Daily" means the operator's calendar day. |
| Snapshot time | the backup time in the snapshot name, set by the client | the server's **commit time** | A host cannot move its snapshots in or out of a period by sending a false clock. |
| Equal snapshot times | impossible (the time names the directory) | possible; ties are sorted by snapshot id, descending, and `keep-last N` still counts N snapshots | Commit times can collide. The order must be total so the preview and the server agree. |
| Newest snapshot | removed if no rule keeps it | never removed | The next incremental backup is based on it. When at least one rule is active, the first rule always keeps the newest unprotected snapshot, so this net never changes a decision. The differential test checks that no decision ever has the reason `newest`. |
| In-progress backups | not counted (unfinished snapshots are skipped) | a snapshot pinned by an open session is treated as protected | GC and prune must never remove a base that a running backup is using. |
| Unknown time zone or policy | — | the whole group is kept, and an error is logged | Failing safe: a configuration mistake never deletes data. |

### Example: the time zone

Two backups at 23:30 UTC on 1 June and at 00:30 UTC on 2 June are on two different days in UTC.
In `America/New_York` they are on the same day (19:30 and 20:30 on 1 June). With `keep-daily 2`,
a UTC policy keeps both. A New York policy keeps only the newer one, plus a snapshot from the
day before if there is one.

## How this is tested

- `internal/retention/pbsref_test.go` holds a literal port of PBS `mark_selections` as the
  reference. It checks every decision and every reason of `Plan` against it:
  - 24 000 random configurations, about 976 000 decisions;
  - 7 time zones, including `Australia/Lord_Howe` (30-minute DST) and `Pacific/Chatham` (+12:45);
  - protected snapshots;
  - snapshots clustered around month and year ends, so ISO weeks straddle the boundary;
  - equal times.

  It also has hand-built cases: ISO week 27 of 2026, ISO week 53 of 2026 going into 2027, and
  hourly backups across the DST changes. Against the code before this change, the test fails on
  ISO week 27.
- `internal/retention/webvectors_test.go` writes and checks `web/src/lib/retention.vectors.json`:
  - 160 random vectors and 60 vectors around month and year ends, across 7 zones;
  - the vitest `web/src/lib/retention.test.ts` requires the TypeScript port to give the same
    decision and reason for every vector;
  - the previous TypeScript port fails against the current vectors.

  To regenerate the vectors, only after changing `Plan` on purpose:
  `IB_WRITE_WEB_VECTORS=1 go test ./internal/retention -run WebConsole`.
