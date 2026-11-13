# Master run 3, 2026-11-13: everything open, integrated

This is the same scenario as runs 1 and 2 (plan 07). It is one run, so the numbers below are single readings, not means.

- **Build 3d3b05e:** a local, never-pushed integration of `fix/sm-master-findings` at **94c1029** (which includes the Netdata restart after M4) with the review fixes on `origin/fix/sm-defects` at **bfff56f** (`b2fdabe`, `22de23a`, `5ba0fd3`, `bfff56f`).
  - A merge of the two conflicted, so the four commits were cherry-picked instead, with trivial resolutions: an import list, two tests added side by side, and one new test's `m.Decide` arity.
  - `go build`, `go vet` and `go test -race -count=1 ./...` pass on the integration.
- **Config and launch:** as in runs 1 and 2: `agent-autonomous.yaml` (rules only, no model), Ollama stopped for the run and restarted at teardown, `setsid master.sh` on `di-node`, and `coldjournal3.sh` for the side read after the cold start.
- **Timetable:** 19:19:42–23:05:15 VM time, `teardown: end`. No kill criterion fired. M7's calm wait lasted 24.6 min (page-hog deleted 21:45:37, relief calm 22:10:12).
- **Readout:** `readout.sh` at 59f58e4. The loops-slowed rate now skips captures from before M5; at 4f90632, M4's forward-stepped captures fell inside its base window and read 408 samples/s.

**Verdicts: 40 pass, 2 fail, 4 not exercised, 6 reported, 1 not built.** Run 1: 31 / 9 / 7 / 5 / 1. Run 2: 38 / 3 / 5 / 6 / 1.

## Every row against runs 1 and 2 (← marks a row that changed)

| Phase | Plan | Criterion | Run 1 | Run 2 | Run 3 |
| --- | --- | --- | --- | --- | --- |
| M0 | 02 | cold start journalled: first non-fact event property.admitted/declared, no restore.failed (inc 1) | pass | pass | pass |
| M0 | 02 | incarnation 1 ids match -1.N; seq monotonic across captures and in the log | pass | pass | pass |
| M0 | 01 | no review decision names a model (answer.by in rules, intervention) | pass | pass | pass |
| M0 | 05 | pace base at start (GET /state .pace) | pass | pass | pass |
| M0 | 04 | self observed: cpu_utilization@unit:di-agent admitted within 1 min | pass | pass | pass |
| M0 | none | healthz reports last tick per loop and last save | not built | not built | not built |
| M1 | 01 | composition cpu_utilization@pod:* -> cpu_utilization confirmed by rules within 2 review cycles | pass | pass | pass |
| M1 | 01 | twins associated by persistent-correlation by t=50 | pass | reported | reported ← |
| M1 | 01 | nothing waits on a model (no model configured; autonomy errors and waiting 0) | pass | pass | pass |
| M1 | 01 | out-of-order and future readings counted (census fields present) | pass | pass | pass |
| M2 | 02 | estimate traceable: kind estimate, no cites, inputs.pace | pass | pass | pass |
| M2 | 06 | interval: sensitivity_low <= sensitivity <= sensitivity_high | pass | pass | pass |
| M2 | 02 | departure: mem-wave stale ~+2 min, retired ~+10 min, its edges retired by cascade | pass | pass | pass |
| M2 | 03 | regime caveat on the next estimate after a relationship.regime | reported | reported | reported |
| M3 | 04 | exactly one collector.down and one collector.up for netdata in 65-80 | fail | pass | pass ← |
| M3 | 04 | Netdata properties unobservable 68-78, none retired; cgroup node properties stay active | fail | pass | pass ← |
| M3 | 04 | relationships with a Netdata end stay active through the outage | not exercised | pass | pass ← |
| M3 | 04 | admission at 72 with Netdata down (decline naming an unobservable reading, or accept when no domain reads Netdata) | reported | reported | reported |
| M3 | 04 | Netdata properties active again within 2 ticks (10 s) of netdata start | fail | pass | pass ← |
| M4 | 01 | two clock.stepped, step (wall_delta - monotonic_delta) ~ +7200 then ~ -7200 | pass | pass | pass |
| M4 | 01 | nothing retires across the steps (80-86) | pass | pass | pass |
| M4 | 01 | noise deleted at 86 goes stale ~2 min later (monotonic sweep) | pass | pass | pass |
| M4 | 01 | relief freshness unaffected: no relief.phase change across the steps | pass | pass | pass |
| M5 | 02 | at most one save interval lost at kill -9 | pass | pass | pass |
| M5 | 02 | incarnation advances 2, 3, 4 and decision ids never collide | pass | pass | pass |
| M5 | 01 | grace: nothing retires in the first retire_after (10 min) after each restart | pass | pass | pass |
| M5 | 02 | learned state survives kill -9 (relief counters, calibration, peers, admission errors) | pass | pass | pass |
| M5 | 02 | pending accept at the kill closes confounded "agent restarted" | not exercised | not exercised | not exercised |
| M5 | 04 | restart at 94 restores state.json.prev (agent log), no restore.failed in inc 3 | pass | pass | pass |
| M5 | 04 | cold start at 97 journalled: first event restore.failed with both errors (GET /state/journal); ledger reconciled | pass | pass | pass |
| M5 | 06 | the cold start's restore.failed reaches the durable log | fail | pass | pass ← |
| M5 | 02 | a cap in force at the kill is resumed, not duplicated | not exercised | not exercised | not exercised |
| M6 | 02 | relief acts and verifies: >= 1 act, each with a later verify citing it | pass | pass | pass |
| M6 | 02 | relief.phase calm->relieving before the first act; relieving->releasing after | pass | pass | pass |
| M6 | 02 | chain resolves: ?chain=1 on the last verify, cites[0] is its act | pass | pass | pass |
| M6 | 03 | calibration fed: relief.memory.high entry n >= 1, hit_rate in [0,1]; verify carries inputs.calibration | pass | pass | pass |
| M6 | 03 | intervention learning: page-hog memory -> memory_contention_ratio provenance intervened | not exercised | not exercised | not exercised |
| M6 | 06 | narrative: explain is text/plain, names the subject and the record's predicted and observed numbers | pass | pass | pass |
| M6 | 06 | durable log: every captured decision id on a digest-verified line; an evicted id answers from: log | pass | pass | pass |
| M6 | 01 | no operator: zero relief.paused, zero relationship.asserted by operator | pass | pass | pass |
| M7 | 02 | each accept has an outcome citing it, admission.arrived between | not exercised | pass | pass ← |
| M7 | 03 | both bases recorded: usage_rise always, map_rise when a measured effect exists | not exercised | not exercised | not exercised |
| M7 | 03 | calibration per domain: admission.memory/cpu_contention_ratio entries, n = scored outcomes | not exercised | pass | pass ← |
| M7 | 03 | 6th accept: basis_chosen_by calibration once both bases have n >= 5, else default | reported | reported | reported |
| M7 | 02 | declines traceable: kind admission, reason named, gate cited | fail | not exercised | pass ← |
| M8 | 05 | one base->slow at 175 + 10 min (+- 1 tick), reason names calm | fail | fail | fail |
| M8 | 05 | loops slowed: collection rate under slow ~1/4 of base (cgroup samples per second) | fail | pass | pass ← |
| M8 | 05 | an estimate at 188 records inputs.pace slow | fail | fail | pass ← |
| M8 | 05 | wake on arrival: slow->base within a tick of noise admitted, reason property admitted | fail | fail | fail |
| M8 | 05 | staleness does not follow the pace: deleted pods stale ~+2 min, retired ~+10 min | pass | pass | pass |
| M9 | 03 | sign contradiction acted on (relationship.retired sign-contradicted) | reported | reported | reported |
| M9 | 03 | regime events | reported | reported | reported |
| M9 | 04 | compaction: no property.forgotten in 3 h; snapshot properties = active + stale + unobservable + retired | pass | pass | pass |

## Every row that is not a pass, checked against its evidence

Categories: (a) agent defect, (b) harness, timing or readout artefact, (c) a case this run cannot produce.

| Row | Verdict | Category | Evidence |
| --- | --- | --- | --- |
| M8 slow at 175 + 10 min | fail | **(b)** | `pace.changed base→slow "calm for 10m0s"` at 22:43:44, 3.5 min after the pods were deleted (22:40:17). Calm had begun before the deletion: the last admission was 22:31:01 (probe-6's pod). Deleting pods is not an attention event, by design. The criterion assumes calm starts at 175. |
| M8 wake on noise's arrival | fail | **(a), fixed in 36c76d6** | The rules confirmed `cpu_utilization@unit:di-agent → cpu_utilization` at 22:51:46 (`review-4.982`). Yet `slow→base "review due"` came at 22:52:44. `Status.Due` still counted the candidate that same cycle had settled, and at the slow pace the next cycle was 4 min away. So the pace was already at base when noise arrived (22:54:48), and there was nothing to wake. Fixed with `TestDueLeavesOutWhatTheCycleSettled` (failing first). No new full run. |
| M5 pending accept at the kill | not exercised | (c) | Nothing was pending at 87. The only earlier offer, `offer-m3` at 72, was declined (`admit-1.502`). `TestAnAcceptIsClosedByARestartOnce` covers the double close. |
| M5 cap in force at the kill | not exercised | (c) | Relief acts only in M6, after the kill. |
| M6 intervention edge | not exercised | (c) | There was one `relationship.intervention`. The intervened provenance needs two agreeing outcomes, and relief acted once. |
| M7 map_rise on scored outcomes | not exercised | (c) | No measured effect exists, because the M6 intervention edge was never built. Every accept has `MapPred: null`. The two held outcomes carry `usage_rise` in both domains. |
| M1 twins associated by t=50 | reported | (b), a design interaction | The pace went slow at 19:50:42 (`calm for 10m0s`), inside M1. Pace inputs, as plan 05 defines them, are relief phase, pending accepts, review due and the last admission; a candidate still filling its persistence windows is none of these. Collection ×4 slower filled the twins' third window later, and they were declared at seq 441 (~20:24), 15 min after M1 ended. Run 1 never slowed, so this stayed hidden. Whether discovery in progress should hold the pace at base is a design question. |
| M3 admission at 72 | reported | by design | Declined: `cpu_contention_ratio would reach 0.1187 on the usage basis, above exit 0.1000`. It cites `admit-1.502-gate` and records `inputs.gate`. The criterion accepts either verdict. |
| M7 6th accept basis_chosen_by | reported | (b) | `default`, because n = 2 per domain, short of 5. Four of the six outcomes were `confounded: another workload moved: pod:f21cca0b…`, which is `cycler`, the M1 discovery workload still cycling during M7. |
| M2/M9 regime events (33), M9 sign-contradicted (1) | reported | by design | Reported only. The sign retirement was again a pod-memory → node-memory composition edge (24 agreements, 156 conflicts). That edge is not exempt; see #78, item 6. |
| M0 healthz liveness | not built | (c) | No plan in 01–06 includes it. |

## Passing rows read real evidence (spot checks)

- **M3:** the log has `inc 1 seq 446 collector.down` at 20:24:46, with `reason: "http://localhost:19999 is unreachable: … connection refused"`, and `seq 521 collector.up` at 20:37:48. 155 Netdata properties were `unobservable` across 20 captures.
- **M4:** two `clock.stepped` events: wall 7259.985 s against monotonic 59.985 s (+7200.000), and wall −7140.004 s against monotonic 59.999 s (−7200.003). `readings_future` stayed **0** for the whole run. Netdata restarted after the step (`ran: systemctl restart netdata.service`, exit 0), so the stale-stamp artefact of runs 1 and 2 is gone.
- **M5:** `journal.ndjson` has `inc 4 seq 1 restore.failed` ("decoding snapshot /var/lib/di-agent/state.json: unexpected end of JSON input"). After the 94 restart the agent log reads "restored 334 properties and 35 relationships from /var/lib/di-agent/state.json.prev".
- **M7:** probe-1 was accepted as `admit-4.497` (seq 499, citing `admit-4.497-gate` at seq 498). Then `admission.arrived probe-1` (seq 513, 22:10:59), then outcome `admit-4.516` citing `admit-4.497`. The held outcome `admit-4.588` scores `usage_err` in both domains, and calibration n = 2 per domain equals those two held outcomes.
- **M7 declines:** the one decline cites its gate, as listed under M3 above.
