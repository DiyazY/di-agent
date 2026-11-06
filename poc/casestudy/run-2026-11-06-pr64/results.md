# Case study P4 expecting no CPU relief, 2026-11-06

This run used `verify.sh` (P3–P5) with the P4 from PR #64, which checks that `cpu.limit` stays calm beside `hog` and `victim` (both requesting 100m). It ran on the same node as [run 2026-11-05](../run-2026-11-05-victim-req3/results.md).
- **Agent:** `f5a96fe`, which is `main` after PRs #58–#63.
- **Raw files:** on the node, in `/home/ubuntu/casestudy/run-2026-11-06-pr64/` (`phases.jsonl`).
- **Times:** EET.

**Outcome: `cpu.limit` stayed calm with no actions at both checks, node contention was about 0 while pressure was 0.28–0.29, and P5 cleaned up.**

| check | time | lever | node `cpu_contention_ratio` | node `cpu_pressure_ratio` |
|---|---|---|---|---|
| five minutes in | 09:04:52 | calm, 0 actions | 1.3e-7 | 0.280 |
| at steady state | 09:27:52 | calm, 0 actions | 2.3e-34 | 0.291 |

The timetable ran in order: P3 at 08:59:51, `hog` and `victim` applied at 08:59:52, P5 deleted them at 09:45:24, and the run ended at 09:54:53. Neither lever acted (`memory.high` calm, 0 actions). This run used the first version of the check, which recorded the lever but kept its verdict on stderr; the review of #64 moved the verdict into the event text.
