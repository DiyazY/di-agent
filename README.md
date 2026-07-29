# di-agent

[![CI](https://github.com/DiyazY/di-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/DiyazY/di-agent/actions/workflows/ci.yml)

**Context-Aware Agent for Decentralized Edge Orchestration**

Implementation artifact for the dissertation *A Context-Aware Agentic Framework for Decentralized Edge Computing* (Tampere University, Diyaz Yakubov, supervised by David Hästbacka and Sergio Moreschini). Funded by Business Finland — Industry X and 6GSoft projects.

---

## What this is

`di-agent` is an autonomous orchestration agent designed for resource-constrained edge nodes (Raspberry Pi 4 and equivalent). It makes context-aware task scheduling and offloading decisions without a centralised control plane.

The agent's decision logic is driven by the **Semantic Map** — an adaptive behavioral data structure that starts from scientifically grounded priors (derived from five empirical publications) and continuously refines its knowledge with live deployment telemetry. As observations accumulate, decisions shift from generic literature knowledge toward deployment-specific behavioral patterns, without labeling or manual tuning.

The implementation is organized around a contract-based architecture: six swappable interfaces (Collector, Storage, Ontology, Updater, Reasoner, Proposer) allow the same agent binary to run on a Raspberry Pi (`edge-minimal` profile) or a cloud VM (`cloud-full` profile) by changing a single startup flag.

---

## Research context

This repository covers **Publications P6 and P7** of the dissertation:

| Publication | Title | Status |
|-------------|-------|--------|
| P6 | Semantic Map & Context-Aware Agent Architecture | In progress |
| P7 | Decentralized Agentic Framework (P2P coordination) | Planned — Year 3 |

The agent synthesises findings from five prior publications:

| Publication | What it contributes to di-agent |
|-------------|--------------------------------|
| P1 — [Performance & Resource Efficiency](https://link.springer.com/chapter/10.1007/978-3-031-84617-5_7) | Initial priors: pod-startup latency, throughput constants per distribution |
| P2 — [Security, Resilience & Maintainability](https://link.springer.com/chapter/10.1007/978-3-031-84617-5_8) | Initial priors: security compliance scores, recovery time constants |
| P3 — Di-Select Orchestration Selection Framework | Backbone topology: 7 constructs, 15 causal propositions (P1–P15) |
| P4 — Energy Analysis (DVFS / CPU microarchitecture) | Initial priors: J/pod, mJ/op, interrupt amplification ratios |
| P5 — Overhead Decomposition (k0s cgroup analysis) | Initial priors: per-component CPU overhead fractions |

---

## Related repositories

| Repository | Description |
|------------|-------------|
| [DiyazY/iot-edge](https://github.com/DiyazY/iot-edge) | Experimental infrastructure for P1 & P2 — Ansible playbooks, k-bench configs, raw benchmark results across 5 Kubernetes distributions on an RPi4 cluster |
| [DiyazY/di-select](https://github.com/DiyazY/di-select) | Grounded theory analysis for P3 — open/axial/selective coding, 15 causal propositions, trade-off patterns |
| [DiyazY/KD-Select](https://github.com/DiyazY/KD-Select) | Extended Di-Select for P3 — AI-driven implementation design, Neo4j schema, RAG pipeline, scraped knowledge base |

---

## Dissertation arc

```
Phase 1 — Foundation (complete)       Phase 2 — Agent's Brain      Phase 3 — Decentralized
──────────────────────────────────    ─────────────────────────    ───────────────────────
P1: Performance benchmarks            P6: Semantic Map +            P7: P2P coordination
P2: Security & resilience                 Context-Aware Agent           Gossip discovery
P3: Di-Select selection framework         (this repo)                   Market-based offload
P4: Energy (DVFS model)                                                 Fault-tolerant handoff
P5: Overhead decomposition
```

---

## Status

Early implementation — contracts, edge-minimal daemon, and prior initialization pipeline are in active development. Code will be published here as P6 matures toward submission.

## Quick start

Go 1.22+ is the only hard requirement.

```bash
cd semantic-map/go
go build ./... && go test ./...     # verify the checkout

./dev.sh demo              # 3-minute guided tour: build, graph, 12 scenarios, UI
./dev.sh replay list       # inventory of the dissertation's 225 Netdata parquets
./dev.sh replay run --kd k0s --test idle --run 1 --speed 0   # replay one parquet
```

## Documentation

| Document | Read it for |
| -------- | ----------- |
| [`DEVELOPING.md`](DEVELOPING.md) | **Changing code?** Install, the inner loop, how to extend (collectors, MetricTypes, contracts, profiles, endpoints, explain tools), testing, conventions, troubleshooting. |
| [`OPERATING.md`](OPERATING.md) | **Deploying it?** Security posture (read this first — there is no auth), resource footprint, systemd unit, health checks, monitoring, multi-node setup, upgrades, what it touches on your system. |
| [`semantic-map/ARCHITECTURE.md`](semantic-map/ARCHITECTURE.md) | The design record — layer map, component reference, request lifecycles, the six contracts and why, multigraph backbone, coordination, the natural-language layer. |
| [`semantic-map/README.md`](semantic-map/README.md) | Operational reference — project structure, endpoint table, daemon flags, compliance tests, prior initialization, PoC quickstart. |
