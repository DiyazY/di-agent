# Memory relief rerun with the lowered lever, 2026-10-30 (b)

This run used `memrelief.sh 45 10` on the same node, with the same workload as [run-2026-10-30-memrelief](../run-2026-10-30-memrelief/results.md).
- **Agent:** branch `feat/memory-refaults` at `c461f69`, with the memory lever's defaults `enter` 0.08, `exit` 0.03 and `min_effect` 0.01. The ldflags named the wrong package, so `/version` shows no commit.
- **Raw files:** on the node, in `/home/ubuntu/casestudy/run-2026-10-30b-memrelief/`.
- **Times:** UTC.

**Outcome: the workload did not reproduce the contention, so the lowered lever was not exercised.** Both levers stayed `calm`, with 0 actions and 0 refusals.

- **page-hog:** it behaved as before. MemAvailable fell to 15% by 09:42, and the hog held 4 files (1 GiB) from 09:44, after a pass that refaulted 51314 pages.
- **mem-victim:** it refaulted only briefly, at 0.002/s around 09:45 with `io_pressure_ratio` 0.002. After that it read 0 for the rest of phase 1: the kernel evicted the hog's own file set and kept the victim's.
- **`memory_contention_ratio`:** it peaked at 0.0024, and was otherwise near 0.
- **Phase 2:** mem-squeeze alone left the signal at 0, with no action.

**What this means:** the earlier run's ~0.11 depended on which file set MGLRU aged out first, and this workload does not control that. To test the lever, the workload has to make the victim's own cache the one that is evicted.
