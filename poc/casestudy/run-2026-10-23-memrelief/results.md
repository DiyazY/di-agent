# Memory relief on di-node, 2026-10-23

`memrelief.sh` on the k0s VM (Ubuntu 24.04 arm64, kernel 6.8.0-147, MGLRU on, no swap, cgroup v2,
cgroupfs driver, MemoryQoS off), agent `ef9db5f` (branch `feat/actuators`) on `agent-relief.yaml`
with both levers at their defaults and the ledger at `<run>/actions.json`. Times are EEST.
Raw files stay on the node, at `/home/ubuntu/casestudy/run-2026-10-23-memrelief{,-2,-3}/`
(`poll.jsonl`, `node.jsonl`, `phases.jsonl`, `agent.log`, `agent.yaml`, `page-hog.log`,
`replaced-service.txt`).

**Outcome: the memory lever never acted, because its trigger condition never arose.** In three runs the
node's `memory_contention_ratio` peaked at 0.00055 (run 1), 0.0049 (run 2) and 0.0012 (run 3), against an
`enter` of 0.20. Both levers stayed `calm` throughout, with 0 actions, 0 refusals and no ledger entries
(no `actions.json` was written). That is the agent doing what it should with what it read. The step the
brief expected (memory.high on page-hog, kept, contention lower after settle) is **not demonstrated** on
the VM. Phase 2 (a self-limited pod triggers nothing) **is**.

Preconditions, checked before each run: every `memory.high` under `/sys/fs/cgroup/kubepods` was `max`, so
the actuator built. No kernel OOM kill happened during any run, and k0s, Ollama and the agent stayed healthy.
The service this replaced (`agent -config agent-demo.yaml`, started 09:33) is recorded in run 1's
`replaced-service.txt`.

## Runs

| Run | Window | page-hog | mem-victim | Node | Max `memory_contention_ratio` |
|---|---|---|---|---|---|
| 1 | P1 14:56–15:42 | file set +256 MiB/min, re-read whole, no anon. Its cache plateaued at 7.8 GB from about 15:25 | 400 MiB file re-read every 1 s, request 512Mi. 412 → 409 MiB resident, memory PSI 0 | MemAvailable 8.5 GB throughout (page cache counts as available) | 0.00055 |
| 2 | P1 18:18–18:49 | 4 GiB anonymous held, then the same file set. Its cache plateaued at about 3.7 GB, set 7.4 GiB | as run 1. 412 → 405 MiB, memory PSI 0 | MemAvailable 4.4 GB (27%): the 5% hold never fired | 0.0049 |
| 3 | P1 18:58–19:29 | 6 GiB anonymous, then the file set, held at 2 GiB once a pass refaulted 10% of it (19:05:25) | 1 GiB file re-read every 1 s, request 1280Mi. **1054 MiB resident at 19:01, then 68–116 MiB from 19:07 to 19:25** | MemAvailable 2.4 GB (15%) | 0.0012 |

The workload changes, each made because the previous run showed why it did not produce the condition:

- **Run 1 → 2 (controller ruling R18):** page cache alone leaves MemAvailable untouched and is the first
  thing reclaimed, so page-hog now holds an anonymous block (4 GiB, `ANON_GIB`). On a swapless node that
  block cannot be reclaimed, so it shrinks the room left for page cache.
- **Run 2 → 3:** the hog kept growing past the room into a streaming reader. A streaming reader's pages
  are each touched once per residency, and MGLRU evicts them before a re-reader's pages, so the victim kept
  its cache. Three changes followed. The anonymous block went to 6 GiB. The hog now stops growing for good
  once a pass refaults a tenth of its set, so its set stays just past what fits and stays hot. The victim was
  enlarged to 1 GiB, request 1280Mi (allowed by R18).

## Why the trigger never fired: the victim's loss of memory is I/O pressure, not memory pressure

Run 3 did produce the intended contention. The protected mem-victim lost about 90% of its working set to
page-hog's pages, and it refaulted continuously. Its pod cgroup at 19:05:53:

| | mem-victim | page-hog |
|---|---|---|
| `workingset_refault_file` | 4,900,760 | 513,765 |
| `workingset_activate_file` | 4,749,644 | 480,401 |
| `workingset_restore_file` | **0** | 61,738 |
| `memory.pressure` some | **avg60 0.00, total 0 µs** | avg10 4.45 |
| `io.pressure` some | **avg60 14.27** | avg60 6.51 |

The kernel counts a refault as a memory stall (PSI memory) only when the folio carries `PG_workingset`. On
the MGLRU kernel, that flag is restored only for a page that reached the highest access tier, or was
faulted in through a mapping. `workingset_restore_file` counts exactly those pages, and for the victim it is 0.
The victim reads with `read(2)`, and each of its pages is evicted before it is re-read often enough to reach
that tier. So its thrashing is accounted as I/O stall (`io.pressure` 14%), never as memory stall. The
agent's `memory_contention_ratio` is defined (design §5) as the highest **memory** PSI `some` among
protected pods. It read 0 correctly. This was not a defect in the agent's code, so no code change was made.

What this means for the lever: on this kernel, `memory_contention_ratio` sees protected pods that stall on
memory. That is expected to cover mapped working sets (executables, mmap'd data), reclaim stalls and anonymous
pressure — expected, not observed here: no run produced any of them on a protected pod. It does not see a `read(2)`-based page-cache working set being displaced. The next VM attempt should use a
victim that maps its file (an mmap reader). Whether the signal should also count a protected pod's
`io.pressure`, or its `workingset_refault_file` rate, is a design question for the spec, not a fix for this
branch.

What the real cgroups did show (run 3, `GET /actuators?subject=`). The actuator's checks against the live pods:

- page-hog: memory.high `can: ""` (actionable).
- mem-victim: `within its memory request: protected, not a culprit`.
- coredns: `within its memory request: protected, not a culprit`.
- kube-proxy: `besteffort: no memory promised`.
- kube-router and metrics-server are above their requests (`can: ""`), but the default policy freezes them (`frozen_by: rule 1`).

## Phase 2: mem-squeeze alone (run 1, 15:48–15:58)

mem-wave holds 400 MB under a 600Mi limit and never reaches it, so the self-limited case used `mem-squeeze`
(buffered writes through a 64Mi limit). Over 10 minutes:

- **Its own limit:** mem-squeeze's `memory.events` `max` went from 128 to 316,896. The subject's
  `memory_events_max` read from 220 to 5,174 per window, and its `memory_pressure_ratio` from 0.001 to 0.026.
  `memory_events_high` stayed 0, as expected with no memory.high set, and `memory_oom_kills` stayed 0.
- **The node signal:** `memory_contention_ratio` peaked at 1.4e-12. The pod was excluded as self-limited.
- **The agent:** the memory lever stayed `calm`, with 0 actions.

**Expected and seen: no memory action, contention < 0.20 while the pod's `memory_events_max` rises.**
