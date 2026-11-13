# Master run, 2026-11-13: one node, no model, no person

Plan 07 (`docs/superpowers/plans/2026-11-10-sm-07-master-scenario.md`), one run, readings not means.

- **Build:** `casestudy/sm-master` on `feat/sm-durable-log` at **a301441** (plans 01–06 stacked, unmerged: #71→#76).
  `go test -race ./...` green; 1396 `Test` functions against 1133 on `ba365ae`; the narrated scenarios
  (`TestScenario_Pace`, `_ReflectiveCore`, `_OutageIsNotDeparture`, `_TraceSurvivesRestart`, `_ColdStart`,
  `_DecisionOutlivesTheJournalAndExplainsItself`) present.
- **Config:** `agent-autonomous.yaml` (rules only, no decision or reasoning backend), ledger in the run directory.
  Ollama stopped for the run and restarted after it.
- **Timetable:** `master.sh`, 09:19:46–12:40:18 VM time, `done exit=0`. Every phase ran; no kill criterion fired.
- **Readout:** `readout.sh run-2026-11-13-master --features 01,02,03,04,05,06` → `results-readout.md` (every row with its reading).
  `phases.jsonl` and `master.out` are here; `capture.jsonl` (129 MB), `journal.ndjson` (3 MB), the snapshots, the
  agent log and the M9 reads are on the VM in `/home/ubuntu/master/run-2026-11-13-master/`.

**Verdicts: 31 pass, 9 fail, 7 not exercised, 5 reported, 1 not built, 0 unverifiable.**

| Plan | Pass | Fail | Not exercised / reported |
| --- | --- | --- | --- |
| 01 defects | M0 no model named; M1 counters, composition by rules, twins by persistent-correlation at t=50; M4 two `clock.stepped` (step +7200.0 s, −7200.0 s), nothing retired, `noise` stale +2.5 min; M5 grace after each restart; M6 no operator | — | — |
| 02 traceability | M0 ids `-1.N`, `seq` monotonic; M2 estimate kind/pace; M2 departure (stale +2.5 min, retired +10.5 min, cascade); M5 ids `-2/-3/-4.`, no duplicates, ≤ one save interval lost, learned state equal across kill −9; M6 act→verify, `relief.phase`, `?chain=1` | M7 declines do not cite a gate | pending accept at kill, cap at kill, M7 accepts: not exercised |
| 03 reflective | M6 calibration `relief.memory.high` n=1 hit_rate 1, verify `inputs.calibration` | — | M6 intervention edge, M7 admission calibration: not exercised; sign-contradicted (2), regime (14): reported |
| 04 robustness | M0 self subject `unit:di-agent`; M5 `.prev` restore at 94, `restore.failed` cold start at 97, ledger reconciled; M9 compaction accounting | M3 ×3 (no `collector.down/up`, properties went `stale` not `unobservable`) | M3 relationships: not exercised |
| 05 pace | M8 staleness unaffected by pace | M8 ×4: the pace never went slow | — |
| 06 durable log | M2 interval 0.9647 ≤ 0.9680 ≤ 0.9714; M6 narrative; M6 378 captured ids all on digest-verified lines, first decision answers `from: log` | M5 cold start's `restore.failed` missing from `journal.ndjson` | — |

## Failures, with the evidence

1. **M3 — a Netdata outage is invisible to the collector supervisor (plan 04).** `systemctl stop netdata` exited 0
   at 65; for the 13 minutes after, `GET /collectors` reported member `netdata` `state: ok`, `last_error: ""`, with
   `last_success_unix` advancing every 30 s and `samples` frozen at 118168. So no `collector.down`/`collector.up` was
   journalled, `SetSourceDown` was never called, and the 12 Netdata properties active before the outage went
   `stale` (sweep silence), not `unobservable`. None retired by 78 (the outage was shorter than `retire_after`
   after their last reading plus grace), and they were `active` again 5.5 s after Netdata started. The cgroup
   collector's node properties stayed `active` throughout. The netdata collector appears to return an empty
   collection without an error when the endpoint refuses connections, so the supervisor counts it a success.
2. **M5 — the cold start's `restore.failed` never reaches the durable log (plan 06 × 04).** `GET /state/journal`
   right after the 97 restart shows incarnation 4 starting `seq 1 restore.failed` with both decode errors (pass
   for plan 04), but `journal.ndjson` has no `restore.failed` line; incarnation 4's first logged line is `seq 2
   fact.recorded`. The event is bumped in `Load` before the log file is attached.
3. **M7 — a decline without a gate (plan 02).** All six offers (`probe-1..6`) were declined with `relief active on
   memory_contention_ratio`; each is `kind: admission` with the reason, but `cites` is null and `inputs.gate` is
   absent: the relief-active decline short-circuits before the `-gate` sub-decision is made.
4. **M8 — the pace never left base (plan 05).** No `pace.changed` event in the whole run. After every planted pod was
   deleted at 175, the memory lever was already `calm` (since 12:10), no accept was pending, but `GET /autonomy`
   `due` kept returning non-zero every few minutes (12:17–12:34: 6, 2, 1, 10, 1, 5, 2, 14, 1, 1, 5): the rules-only
   reviewer leaves most candidates `undecided` (348 of 408 reviews), and they come due again as their evidence
   moves, so "review due" resets the calm before ten minutes pass. The estimate at 188 records `pace: base`; the
   wake on `noise` at 190 has nothing to wake from.

## What the run did not exercise, and why

- **M7 accepts:** the memory lever held `releasing` from 11:14 (its cap on `page-hog` kept, verdict `keep`,
  predicted 0.132 / observed 0.131) to 12:10, 25 min after `page-hog` was deleted at 145; admission declines while
  relief is active, so no probe was accepted and no admission outcome, calibration or `basis_chosen_by` could be
  read. The offer at 72 (Netdata down) was accepted, and closed `not_arrived` well before the kill, so no accept
  was pending at 87 either.
- **M6 intervention edge:** one `relationship.intervention` (`agreed`) on `memory_utilization@pod:<page-hog> →
  memory_contention_ratio`; two agreeing outcomes are needed and relief acted once.
- **M3 relationships with a Netdata end:** none existed.

## Readings worth a second look (not criteria)

- `sign-contradicted` retired two relationships, one of them `memory_utilization@pod:<page-hog> →
  memory_utilization` (48 agreements, 132 conflicts) — a composition edge: the pod's memory is part of node memory,
  and the conflicts came while the cap reclaimed the pod's page cache.
- `readings_future` rose from 0 to 183098 over the run (`readings_out_of_order` stayed 0).
- In the scaled dry run, a pending accept at the kill was closed `confounded: agent restarted` twice (once in
  incarnation 2, again in 3 after the `.prev` restore brought it back): two outcomes cite `admit-1.364`.
- The restore log line after a `.prev` restore says "restored … from state.json" while restoring `state.json.prev`.

## Adaptations of the plan's criteria to the build

- Journal file and fields as on the stack: `journal.ndjson`, `GET /state/journal` `log`, explain at
  `GET /state/decisions/{id}/explain`, `pace`/`pace_since`, `unobservable`, `unit:di-agent`.
- Review "by" is `answer.by` (not `Inputs.by`); outcome lines are `map_rise`/`usage_rise`.
- A fresh map's first non-fact events are `property.declared` (derived properties) before `property.admitted`.
- `clock.stepped` reports `wall_delta_seconds` over a sweep interval; the step is `wall_delta − monotonic_delta`.
- `GET /autonomy` has no `models_down`; "nothing waits on a model" read as `errors == 0 and waiting == 0`.
- "Loops slowed" was to be read from the cgroup collector's sample rate (not computable: no slow period).
- `GET /healthz` is `{"ok":true}`; liveness detail is in none of plans 01–06 (`not built`).

## Rulings

1. The VM has no chrony; M4 uses `timedatectl set-ntp false`, `date -s ±2 hours`, `set-ntp true` (systemd-timesyncd).
   The node was `Ready` after each step; NTP synchronised again after the run.
2. Ollama stopped → `page-hog` holds 11 GiB anonymous instead of 6, standing in for the model's 6–9 GB.
3. `kill -9` at 87, not 86: `noise` is deleted at 86 (M4) first.
4. The harness times phases on `/proc/uptime` and stamps each mark with the agent's `(inc, seq)`; windows are read
   by journal position, durations by the events' own clock.
5. `capture.sh` polls `/state/journal?limit=50` (plus `/collectors /autonomy /healthz /tasks /peers`); the complete
   record is `journal.ndjson`, copied at teardown.
6. A side read (`coldjournal.sh`, not committed) saved `GET /state/journal` right after the 97 cold start, since the
   durable log lacks `restore.failed` (failure 2).
7. M9 reads the last verify's chain and narrative and the run's first decision before teardown stops the agent.
8. The dry run (`SCALE=0.05`) used this stack build, not a build with none of the plans.
9. `readout_test.sh` checks a representative set of criteria (cold start, model, pace, self, clock) on a passing
   and a failing hand-written run, not every criterion.
