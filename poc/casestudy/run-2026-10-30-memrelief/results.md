# Memory relief rerun on di-node, 2026-10-30

This run used `memrelief.sh 45 10` on the same k0s VM as the 2026-10-23 runs (Ubuntu 24.04 arm64, kernel 6.8, MGLRU, no swap, cgroup v2, MemoryQoS off).
- **Agent:** branch `feat/memory-refaults` at `aa51315`, built without version ldflags, so `/version` shows no commit. It ran on `agent-relief.yaml` with the default levers (memory: `enter` 0.20, `exit` 0.05).
- **Workload:** the run-3 setup. page-hog held 6 GiB of anonymous memory, then a file set it stopped growing at the first pass that refaulted 10%. mem-victim re-read a 1 GiB file every second, with a request of 1280Mi.
- **Raw files:** these stay on the node, in `/home/ubuntu/casestudy/run-2026-10-30-memrelief/`.
- **Times:** UTC (EET − 2 h).

**Outcome: the signal now sees the contention, but the lever did not act. `memory_contention_ratio` peaked at 0.116, under `enter` 0.20.**

- Both levers stayed `calm` all run: 0 actions, 0 refusals, and no ledger written.
- **Phase 2 is demonstrated.** mem-squeeze alone left `memory_contention_ratio` at 0 and triggered nothing.

## Phase 1 (07:28–08:14)

page-hog filled the room in three minutes: MemAvailable fell from 53% to 15%, and the hog held its set at 4 files (1 GiB) from 07:31. mem-victim kept about 1.07 GB charged to it but refaulted throughout. Its readings, sampled about every 3 minutes:

| | before page-hog | 07:34–08:12 |
|---|---|---|
| `memory_refault_ratio` | 0 | 0.25–0.32 /s (25–30× the 0.01 gate) |
| `io_pressure_ratio` | 0 | 0.10–0.117 |
| `memory_pressure_ratio` | 0 | **0** |
| `memory_events_high` | 0 | 0 (not self-limited) |

- **Node `memory_contention_ratio`:** 0 until 07:31, then a steady 0.098–0.116 until page-hog was deleted. It then decayed back to 0.
- **Comparison with 2026-10-23:** before this change, the same victim's stall read 0. This time the stall was again all I/O, with memory PSI at 0. It now reaches the signal through the refault gate, as designed.

## Phase 2 (08:19–08:30)

mem-squeeze ran alone, limiting itself at its own 64Mi limit. `memory_contention_ratio` stayed at 0, and the memory lever stayed calm with 0 actions.

## What is left

The victim lost about 11% of its time to refault stalls. That is real harm to a protected pod, but it is half the lever's `enter`. The default `enter` of 0.20 was chosen when the signal was memory PSI only, and it was never calibrated against refault-driven io stall. Whether to lower the memory lever's `enter`, or to keep it and treat 11% as below the bar for action, is a policy decision. It is not a defect in the signal.
