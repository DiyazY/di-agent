# Memory relief with a victim-aware page-hog, 2026-10-30 (c)

This run used `memrelief.sh 45 10` on the same node as [run b](../run-2026-10-30b-memrelief/results.md).
- **Agent:** `c461f69`, with memory lever `enter` 0.08, `exit` 0.03 and `min_effect` 0.01.
- **Workload:** the page-hog from `4330ddd`. It grows its file set by one 256 MiB file a minute until mem-victim refaults at 0.05/s or more, and holds while it does.
- **Raw files:** on the node, in `/home/ubuntu/casestudy/run-2026-10-30c-memrelief/`.
- **Times:** EET.

**Outcome: the memory lever relieved the victim. Each cap on page-hog took `memory_contention_ratio` from about 0.09 to about 0 within two minutes, and both caps were kept.** The run also exposed two defects in how relief steps and gives back, below. Phase 2 (mem-squeeze alone) stayed at 0 with no action.

## Timeline

| time | what happened |
|---|---|
| 21:12–21:18 | page-hog takes 6 GiB anonymous memory, then grows to 6 files. mem-victim starts refaulting: 0.13/s, then 0.59/s; `io_pressure_ratio` 0.093; `memory_pressure_ratio` about 0. Node contention reaches 0.086. |
| 21:20:18 | **Step 1:** page-hog `memory.high` 7707 → 5780 MiB (usage basis, predicted drop 0.013). |
| 21:22:33 | Verify: contention went from 0.092 to 0.0009; **kept**. mem-victim's `memory_utilization` recovered from 0.035 to 0.064, so its cache was back. |
| 21:24:15 | Contention stayed under `exit` for 3 min, so the lever gave the cap back (`released`). page-hog, seeing the victim calm, grew to 7 files. |
| 21:25–21:34 | Contention back at 0.085–0.091; the victim refaults at 1.1–1.7/s. page-hog is in cooldown until 21:34. |
| 21:34:18 | **Step 2:** 7963 → 5972 MiB (usage basis, learned effect 2, predicted drop 0.025). Contention fell to 0.0003; **kept**. |
| 21:36:33 | **Step 3:** a further step to 4627 MiB (map basis, predicted drop 0.083), taken at contention 0.0003. Verify **reverted** it: there was nothing left to relieve. |
| 21:38:17 | The lever released both caps. Contention was back at 0.08–0.09 by 21:40. |
| 21:40–21:58 | The lever is relieving but page-hog is in cooldown until 22:18 (40 min after the revert), so no action until page-hog is deleted at 21:58. |

## What the run shows

- **The signal and lever work end to end.** Refault-driven io stall on a protected pod raised `memory_contention_ratio`. The usage-basis plan capped the right pod, and the cap brought the victim's cache back within one settle window. Both kept steps under-predicted their effect: they predicted 0.013 and 0.025, and observed 0.091 and 0.086.
- **Defect 1, fixed in `6e71051`:** a further step was taken at contention 0.0003. While relieving, the loop stepped whenever nothing was in flight. Now a further step needs pressure above `enter`.
- **Defect 2, open:** caps are given back as soon as contention has stayed under `exit` for `exit_for`. But contention is low *because of* the cap, and the culprit has not changed, so contention returns within about two minutes of every release. Over the 46 min of phase 1, the victim was relieved for about 9 (21:21–21:25 and 21:35–21:39). page-hog also grew a seventh file while the first cap held, because it saw the victim calm. The CPU lever releases the same way.
- **Note:** the third step's map-basis prediction (0.083) came from the relationship the map learned between page-hog's usage and contention during steps 1 and 2. It was right about the direction and wrong about the state: by then the cap had already removed the pressure.
