# CPU relief beside a victim, owed contention, 2026-11-05

These runs used P4 of `verify.sh` (hog beside victim) on the same node as [run 2026-11-04f](../run-2026-11-04f-issue56/results.md). They asked why `cpu.limit` never acted, and whether the cause was the build or the measure.
- **Node:** 12 CPUs, with pods held to CPUs 0–5; k0s v1.36.4.
- **Workloads:** `hog`, six busy loops under a 4.5-core limit, and `victim`, three busy loops under a 6-core limit. Both request 100m unless the table says otherwise.
- **Readings:** every 30 s from `/state` (each pod's `cpu_utilization` × 12 cores and its `cpu_contention_ratio`, and the node's) and from the pod cgroups' `cpu.weight`.
- **Raw files:** on the node, in `/home/ubuntu/casestudy/run-2026-11-05-integ/`, `run-2026-11-05-oldbin-cpu/` and `run-2026-11-05-victim-req3/` (the probe lines are in this file).
- **Times:** EET.

**Outcome: in every case the kernel gave `victim` its `cpu.weight` share, nothing was owed, and `cpu.limit` stayed calm. That is correct, so the case study's expectation was wrong, not the build.**

| run | agent | victim request | victim cores | victim contention | hog cores | hog contention | node `cpu_contention_ratio` | node pressure |
|---|---|---|---|---|---|---|---|---|
| integ | `cd09f68` | 100m | ~2.8 | 0.055 | ~2.7 | 0.996 | 0 | 0.30 |
| oldbin-cpu | `25a2aaa` | 100m | — | — | — | — | 0.03 | 0.34 |
| victim-req3 | `f5a96fe` | 3 cores (`cpu.weight` 118, hog 4) | 3.00 | 0.000–0.003 | 2.48–2.85 | 0.47–0.52 | 0–0.011 | 0.16–0.27 |

The victim-req3 probe, one line every 30 s:

```
23:31:12 node contention=0.000 pressure=0.256 util=0.446 | hog cores=2.80 cont=0.473 w=4 | victim cores=2.99 cont=0.002 w=118
23:32:18 node contention=0.000 pressure=0.266 util=0.450 | hog cores=2.85 cont=0.495 w=4 | victim cores=2.99 cont=0.003 w=118
23:33:25 node contention=0.000 pressure=0.258 util=0.454 | hog cores=2.84 cont=0.491 w=4 | victim cores=3.00 cont=0.000 w=118
23:34:30 node contention=0.000 pressure=0.181 util=0.478 | hog cores=2.58 cont=0.504 w=4 | victim cores=3.00 cont=0.000 w=118
23:35:35 node contention=0.002 pressure=0.159 util=0.514 | hog cores=2.48 cont=0.519 w=4 | victim cores=3.00 cont=0.000 w=118
```

## Reading

`owedContention` counts a pod as owed its `cpu.weight` share of the pods' CPUs. Kubernetes derives that weight from the request, and the scheduler enforces it between pods, so the pod gets the share whatever its neighbour runs. `victim` therefore holds what it is owed. All of the node's CPU pressure is `hog` waiting on its own threads past its share, and capping `hog` would only lengthen that wait. The old build (`25a2aaa`) behaved the same way. CPU relief between pods is mostly the kernel's work already; what remains is listed in ARCHITECTURE §16, *Where relief acts*.
