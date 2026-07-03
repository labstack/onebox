# yeet M2 — trustworthy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Design §12 M2: journal (as specified in §05), fencing on every host, lock authority, `resume`/`abort`, the migration-gate protocol (§06), per-role verify, pinned-retention GC, `audit`. Exit = the ops scenarios: kill the runner mid-release → `resume` recovers; fail verify after a migration → halt-and-page, not rollback; break a worker → verify catches it.

**Architecture:** Mechanisms, not claims (design §02). The journal is append-only JSONL at `/var/lib/yeet/<app>/journal/<deploy-id>.jsonl` (deploy id == release id), one `sync` per record, written over the existing Transport. Exclusivity = a noclobber lock file at the authority (v1: the only host) with TTL on the host's clock + heartbeat touch; breaking an unexpired lock needs `--force` and prints the holder + journal tail. Fencing = an epoch counter + a fence file; **every mutating command is wrapped host-side** with a check against the host's own fence — a zombie runner is rejected locally (exit 97), no cross-host call. Resume is journal-driven: completed phases/roles skip; a half-rolled role recovers because newcomers are identifiable by the `yeet.release=<id>` label M0 already injects. The migration gate: `$YEET_RESULT_FILE` exported to the migrate hook; `changed=false` opens the gate; anything else — including nothing — fails safe (schema assumed changed → verify failure halts-and-pages instead of auto-rolling back), unless `migrations: expand-only` is the operator's informed promise. The same gate governs `abort`.

## Global Constraints

- All M0/M1 constraints hold. Injection rules: journal JSON goes through `q()`; ids/epochs validated.
- One regime for every mutation (design §05 rev 5): deploy, resume, abort, rollback, bootstrap all acquire the lock, journal, and fence identically.
- Fence-check exit code is 97; the engine translates it to "fenced: a newer deploy owns this host".
- Verify failure semantics: gate OPEN (migrate no-op or `migrations: expand-only`) and not `--no-rollback` → auto-rollback (replay previous release, newcomers removed); gate CLOSED → **halt-and-page** with the previous release left running and the new release NOT activated.
- Journal outlives its release: GC keeps journals for retained releases + the same count again.
- Hosts are Linux (design §11): `stat -c %Y` is fine.

## Tasks

### Task 1: journal package
- Files: `internal/journal/journal.go`, `journal_test.go`.
- `Record{DeployID, Epoch int, Phase, SubStep, Role, Event (start|intent|result|finish|abort), Status (ok|fail), Detail, TS, Operator, GitSHA, ConfigHash string}`.
- `Writer{T, App, DeployID}`: `Append(ctx, Record)` → fills TS/Operator, marshals one JSON line, `mkdir -p <dir> && printf '%s\n' <q(line)> >> <file> && sync <file>`.
- `Read(ctx, t, app, deployID) []Record` (`cat`, tolerant line parse), `List(ctx, t, app) []string` (newest last), `Summary(records)` → `{Started, Finished, Aborted bool, PrevRelease string, GateOpen bool, Done map[string]bool}` where keys are `"transfer"`, `"migrate"`, `"release:<role>"`.
- Tests: append command shape (mkdir, printf, sync, path), read/parse round trip via Fake, summary over a partial deploy.

### Task 2: lock + epoch + fence
- Files: `internal/engine/lock.go`, `lock_test.go`; `Options.LockTTL` (default 10m).
- `AcquireLock(ctx, meta) (epoch int, err)`: read+bump `/var/lib/yeet/<app>/epoch`; noclobber-create `/var/lib/yeet/<app>/lock` (JSON: owner, deploy_id, epoch, git_sha, config_hash, ttl_s). Held: if `stat -c %Y` age > TTL → journal `lock-takeover`, rm, retry once; else error printing holder (+ `--force` path: rm + print holder's journal tail).
- Heartbeat: goroutine `touch lock` every TTL/10, stopped by context/release. `ReleaseLock` (rm, only if ours).
- Fence: `WriteFence(ctx, id, epoch)` → `echo 'id epoch' > fence`; `e.fence` set; `e.mutate(ctx, cmd)` wraps: `if [ "$(cat <fence> 2>/dev/null)" = "<id> <epoch>" ]; then <cmd>; else echo yeet-fenced >&2; exit 97; fi`; exit 97 → typed error.
- Tests: acquire/held/stale/force sequences; heartbeat commands; mutate wrapping + 97 translation.

### Task 3: fence every mutation + journal the lifecycle + resume-aware rolling
- Files: modify `engine/{deploy,roll,recreate,bootstrap}.go`, `deploy_test.go` additions.
- All mutating commands (compose up/stop/rm/kill/exec-drain, host hooks, activate, prune) go through `e.mutate`. Reads (ps/inspect/cat/df) stay bare.
- `Deploy` core: preflight → AcquireLock + WriteFence + journal `start` (records prev release, config hash, git sha) → phases journal `intent`/`result` per phase and per role → `finish` (or `fail`); lock released on every path; `defer` heartbeat stop.
- RollRole: newcomers found via `--filter label=yeet.release=<id>` (id = release-dir base); "old" = service containers minus newcomers. If a healthy newcomer already exists (resume), skip pull/up and continue from converge/drain. >1 newcomer or >1 old = error.
- Tests: fence wrapper present on every mutating command in a full deploy (grep the Fake log); journal records appear in order (start → migrate result → release:web result → finish); resumed roll skips `up --scale`.

### Task 4: migration gate + auto-rollback/halt-and-page
- Files: `engine/gate.go` inside deploy flow; config `Migrations string` (`""|"expand-only"`) + schema.cue; `--no-rollback` flag → `Options.NoRollback`.
- Migrate hook runs with `YEET_RESULT_FILE=<releaseDir>/.migrate-result` exported; afterwards `cat` it: `changed=false` → gate open; missing/other → closed (fail safe). Journaled in the migrate `result` record.
- On verify failure: gate open (or expand-only) and !NoRollback → auto-rollback: remove newcomers, replay previous release roles, journal `result phase=auto-rollback`; else return the halt-and-page error naming the gate state and the exact reason auto-rollback was refused.
- Tests: three paths — no-op migrate → verify fail → auto-rollback commands present; silent migrate → verify fail → halt error, no rollback commands; expand-only asserted → rollback despite silent migrate.

### Task 5: resume + abort verbs
- Files: `engine/resume.go`, `cmd/yeet/commands.go`.
- `Resume`: newest journal without `finish|abort` → Summary → re-acquire lock (NEW epoch, new fence — the old runner is thereby fenced), skip Done phases/roles, finish normally. Release dir must exist (transfer re-verified by `test -d`).
- `Abort`: same discovery + gate check (gate closed → refuse with halt-and-page text unless `--force`); remove newcomers (`yeet.release=<id>` label), replay previous release for roles whose result was ok, journal `abort`.
- Tests: resume skips completed role + finishes; resume fences the old epoch; abort removes newcomers and refuses when gate closed.

### Task 6: audit + journal GC
- Files: `cmd/yeet/commands.go` (`yeet audit [-n]`), `release.Prune` journal GC.
- Audit: list journals newest-first, Summary each, table: id, operator, git sha, started, outcome (deployed/failed/aborted/incomplete). Includes runs whose terminal scrolled away — that's the point.
- Prune: after release removal, delete journals not among (retained ids + retain extra).
- Tests: audit output over two journals; GC keeps retained + extra.

### Task 7: e2e ops scenarios + docs + merge
- e2e additions (gated as before): (a) kill-runner-mid-release — cancel the deploy context after the newcomer starts, then `Resume` recovers to v2 with zero downtime maintained; (b) break-a-worker — deploy a role whose ready.exec fails → deploy halts, old serving.
- README M2 section; full suite; live e2e; merge --no-ff to main.

## Self-Review
M2 roadmap line coverage: journal ✔(1) fencing ✔(2,3) lock authority ✔(2) resume/abort ✔(5) migration gate ✔(4) per-role verify incl. drain ✔(M0/M1 + exit test 7b) pinned retention ✔(6 GC; images already never pruned) audit ✔(6). Exit scenarios mapped to tests: kill→resume (7a), migrate+verify-fail→halt (4), broken worker (7b). Not ahead of scope: no multi-host fan-out, no status verb, no accessory/proxy apply.
