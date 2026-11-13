# Master run 2, 2026-11-13: the fixes of #78

This is the same scenario as `run-2026-11-13-master` (plan 07). It is one run, so the numbers below are single readings, not means.

- **Build:** `fix/sm-master-findings` at **37bb692**, on `casestudy/sm-master` (#71→#77). `go test -race ./...` passes.
- **Config and launch:** as in run 1: `agent-autonomous.yaml` (rules only, no model), with Ollama stopped for the run and restarted at teardown. `setsid master.sh` ran on `di-node`. `coldjournal2.sh` made the same side read of `GET /state/journal` after the 97 cold start. `master.sh` included the M7 calm wait (item 7). It did not yet include the Netdata restart after the M4 step, which was added after this run.
- **Timetable:** 13:58:29–17:44:02 VM time, `teardown: end`. Every phase ran and no kill criterion fired. M7's calm wait lasted 24.6 min: page-hog was deleted at 16:24:27 and relief reported calm at 16:49:01. Everything after M7 ran 24 min late.
- **Readout:** `readout.sh run-2026-11-13-master-2 --features 01,02,03,04,05,06` → `results-readout.md`. Two readout fixes come with this run: outcome lines are read from `inputs.lines`, and only scored (held/breached) outcomes count for bases and calibration.

**Verdicts: 38 pass, 3 fail, 5 not exercised, 6 reported, 1 not built** (run 1: 31 / 9 / 7 / 5 / 1).

## Rows that changed against run 1

| Row | Run 1 → run 2 | Reading |
| --- | --- | --- |
| M3 one collector.down/up for netdata | fail → **pass** | `collector.down` carries the reason `…dial tcp [::1]:19999: connect: connection refused`; `collector.up` came 4.6 s after `systemctl start` |
| M3 Netdata properties unobservable, none retired | fail → **pass** | 155 Netdata properties, 20 captures, all `unobservable`, none retired; the cgroup node properties stayed active |
| M3 relationships with a Netdata end stay active | not exercised → **pass** | 160 relationship readings, all active (the per-core → system composition edges) |
| M3 Netdata active again within 2 ticks | fail → **pass** | `collector.up` 4.6 s after start |
| M5 restore.failed reaches the durable log | fail → **pass** | `journal.ndjson` inc 4 starts at `seq 1 restore.failed` |
| M7 each accept has an outcome citing it | not exercised → **pass** | 6 accepts, 6 outcomes: 2 held, 4 confounded (`another workload moved`: mem-victim) |
| M7 calibration per domain | not exercised → **pass** | `admission.cpu_contention_ratio` and `admission.memory_contention_ratio`, `usage` n = 2 each = scored outcomes |
| M8 loops slowed | fail → **pass** | cgroup sample rate 31.2/s at base, 7.5/s at slow (×4.15), measured in the slow period of 14:30–15:10 |
| M7 declines cite a gate | fail → **not exercised** | no decline in this run: all six probes were accepted. The fix is covered by `TestReliefActiveDeclines` |
| M1 twins associated by t=50 | pass → **reported** | declared after M1 ended. The pace went slow at 14:30, during M1, once no rule-answerable work was due. That slowed collection ×4, so the twins' persistent windows filled 4× slower |

The rows not listed above kept their verdict. These include the M5 `.prev` restore: the agent log now says "restored … from /var/lib/di-agent/state.json.prev".

## Still failing: M8 (3 rows), with the cause

The pace did slow under rules only in this run (`base → slow: calm for 10m0s` at 14:30; run 1 had no `pace.changed` at all). It did not slow in M8, because of the M4 clock step. Netdata keeps the `last_updated` of the forward-stepped clock until the wall clock catches up, which took 2 h, from 15:22:30 to 17:22:30. Until then the agent refused its readings as future: `readings_future` was 0 through M3, then rose to 191,329. The 97 cold start had given the agent a new map, so at 17:22:30, 3 min into M8, 143 Netdata properties (mostly the per-core `cpu.cpu.*@cpu:cpuN`) were admitted all at once at 17:22:30, 3 min into M8. Each admission woke the pace. Then rule-answerable composition candidates made review due: `disk.io.writes@disk:sda → system.io.out` at 17:26:38, and `k0scontroller.service` cpu → node at 17:29:38. So the 10 min calm never accrued before noise arrived at 17:33.

`master.sh` now restarts Netdata right after the backward step. That is part of undoing the fault, not agent logic. A full run with that change has not been done.

## Not exercised, and why

- **M5 pending accept / cap at the kill:** nothing was pending at 87, and no cap was in force. `TestAnAcceptIsClosedByARestartOnce` covers the double-close fix.
- **M6 intervention edge:** one intervention, `agreed`; the edge needs two.
- **M7 map_rise:** no measured effect exists (the intervention edge was not built), so only the `usage` basis predicts.
- **M7 declines:** none were made.

## Other readings

- **Doctor:** no `collector.doctor`, because the built-in Netdata collector declares none. This is as designed.
- **`sign-contradicted`:** one retirement in this run (two in run 1).
