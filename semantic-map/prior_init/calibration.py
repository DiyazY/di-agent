"""
Proposition and construct calibration.

For each of the 15 Di-Select propositions, we compute a PriorStrength λ ∈ [0,1]
using the Spearman rank correlation between the source construct proxy score and
the target construct proxy score across the 5 benchmarked distributions.

Methodology
-----------
1. Assign each distribution a normalised score [0,1] on each construct,
   derived from published empirical constants and computed CSV results.
2. For each proposition (FromConstruct → ToConstruct, Positive|Negative):
   - Compute Spearman ρ between from-scores and to-scores.
   - λ = |ρ|  (strength only; direction is fixed by the proposition polarity).
   - Clip to [0.30, 0.90] to avoid extreme cold-start weights.
3. Return per-distribution construct scores and per-proposition λ values.

Construct proxy variables
-------------------------
PS  – Performance & Scalability:  inverted pod-startup latency + throughput
SC  – Security & Compliance:       CIS security score
RR  – Reliability & Resilience:    inverted recovery time + offline preservation
MU  – Maintainability & Usability: inverted setup time
RC  – Resource Constraints & Cost: inverted energy_per_pod_j + inverted cp_overhead_w
CO  – Connectivity & Offline:      offline_preservation + inverted cp_amplification
CE  – Community & Ecosystem:       normalised GitHub stars
"""

from __future__ import annotations

from scipy.stats import spearmanr

from .constants import (
    SECURITY_SCORES,
    POD_STARTUP_LATENCY_MS,
    DP_THROUGHPUT_OPS,
    SETUP_TIME_HOURS,
    RECOVERY_TIME_MIN,
    OFFLINE_PRESERVATION,
    GITHUB_STARS,
)
from .loaders import load_energy_efficiency, load_interrupt_amplification

KDS = ["k3s", "k0s", "k8s", "kubeEdge", "openYurt"]

# Propositions: (id, from, to, direction)
# Constructs the agent can observe at runtime. A construct that cannot change
# while a cluster runs is not state, and an edge with no observable endpoint is
# provably inert in the Reasoner: cost accumulates (effective - prior), and an
# unobserved edge has effective == prior by definition, so it contributes exactly
# zero regardless of its prior or of any operator action on it.
#
# Membership is a data decision, not a code one. Routing a new MetricType to a
# construct (see the Bridge's routing table) is what makes it admissible here;
# adding it to this set and regenerating is the whole change.
TELEMETRY_CONSTRUCTS = ["RC", "CO", "PS"]

CONSTRUCT_META = {
    "RC": ("Resource Constraints & Cost",
           "CPU, memory and energy cost of running the workload"),
    "CO": ("Connectivity & Offline Resilience",
           "network throughput, loss and latency to peers"),
    "PS": ("Performance & Scalability",
           "scheduling and startup latency; resource pressure experienced"),
    "SC": ("Security & Compliance", "hardening posture; not runtime state"),
    "RR": ("Reliability & Resilience", "recovery behaviour; runtime but unrouted"),
    "MU": ("Maintainability & Usability", "operational effort; not runtime state"),
    "CE": ("Community & Ecosystem", "vendor backing; not runtime state"),
}

# The full Di-Select backbone, retained as the source from which the agent's
# graph is derived. Di-Select remains the origin of the causal claims; the agent
# instantiates the subset it can actually observe.
DISELECT_PROPOSITIONS = [
    ("P1",  "SC", "RC", "positive"),
    ("P2",  "RC", "PS", "negative"),
    ("P3",  "RC", "PS", "positive"),
    ("P4",  "SC", "RR", "negative"),
    ("P5",  "CO", "RR", "positive"),
    ("P6",  "CO", "RR", "negative"),
    ("P7",  "CE", "MU", "positive"),
    ("P8",  "MU", "RC", "negative"),
    ("P9",  "CE", "MU", "negative"),
    ("P10", "PS", "RC", "negative"),
    ("P11", "CE", "SC", "positive"),
    ("P12", "SC", "MU", "negative"),
    ("P13", "CO", "PS", "negative"),
    ("P14", "RC", "SC", "negative"),
    ("P15", "MU", "RR", "positive"),
]

# The agent's graph: propositions whose BOTH endpoints are observable. An edge
# with one observable endpoint still receives observations through it, but its
# far construct is a constant, so the edge cannot express a relation — see
# research-docs/relational-edge-learning.md.
PROPOSITIONS = [p for p in DISELECT_PROPOSITIONS
                if p[1] in TELEMETRY_CONSTRUCTS and p[2] in TELEMETRY_CONSTRUCTS]


def _norm_inv(vals: dict[str, float]) -> dict[str, float]:
    """Normalise then invert: higher raw = lower score (e.g. latency → perf score)."""
    lo, hi = min(vals.values()), max(vals.values())
    span = hi - lo or 1.0
    return {k: 1.0 - (v - lo) / span for k, v in vals.items()}


def _norm(vals: dict[str, float]) -> dict[str, float]:
    """Min-max normalise: higher raw = higher score."""
    lo, hi = min(vals.values()), max(vals.values())
    span = hi - lo or 1.0
    return {k: (v - lo) / span for k, v in vals.items()}


def _blend(a: dict[str, float], b: dict[str, float], w_a: float = 0.5) -> dict[str, float]:
    """Weighted average of two normalised score dicts."""
    return {k: w_a * a[k] + (1 - w_a) * b[k] for k in KDS}


def _spearman_strength(x: list[float], y: list[float]) -> float:
    """Return |Spearman ρ|, clipped to [0.30, 0.90]."""
    rho, _ = spearmanr(x, y)
    return float(max(0.30, min(0.90, abs(rho))))


# ── Machine classes ──────────────────────────────────────────────────────────
#
# A cluster's machines are not interchangeable, and calibrating them as if they
# were misattributes measurements. On the dissertation testbed the control-plane
# host is an x86 NUC (i7-10710U, 64 GB) and the workers are ARM RPi4s
# (Cortex-A72, 8 GB): different instruction set, an order of magnitude of memory,
# and different roles.
#
# The per-class scores below are a CORRECTION rather than a re-fit. Each source
# constant is routed to the class it was actually measured on, and where no
# measurement exists for a class the score is inherited from the cross-class
# figure and said to be inherited. Nothing is re-derived from the same telemetry
# the agent later learns from, which would make the prior a summary of the
# evaluation data.
#
# Provenance per construct, from the sources in constants.py and loaders.py:
#
#   RC  worker only. P4's energy model is an ARM Cortex-A72 / BCM2711 DVFS model
#       ("on RPi4 worker nodes"), and P5's overhead decomposition is cgroup data
#       from node_2. Both quantities that feed RC therefore describe a worker.
#       There is no control-plane-side energy measurement in the campaign, so the
#       control-plane class inherits — and the gap is recorded rather than hidden
#       behind a uniform prior.
#
#   PS  worker only, after measurement corrected a first attempt. The test for a
#       class-specific score is whether the constant measures THAT CLASS'S LOCAL
#       BEHAVIOUR — not merely whether the class participates in the operation. Data
#       -plane throughput passes: memtier drives Redis processes that execute on
#       workers. Pod-startup latency fails: it is timed end to end across client,
#       API server, scheduler, kubelet and container runtime, so it characterises a
#       cluster-wide path rather than the control-plane host's local resource
#       behaviour.
#
#       The first version of this file routed latency to the control-plane class on
#       provenance grounds alone, and the A/B measured the consequence: on all nine
#       k0s workloads the master's divergence from its prior GREW by 6.2-9.5%, while
#       the worker's shrank by 6.3-8.8% from the throughput-derived score. More
#       specific provenance is not automatically a better prior.
#
#   CO  cross-class. Offline message preservation is architectural — a property of
#       the distribution's edge design, not of a host — and the interrupt
#       amplification figures are not resolved per host in P4's results.
MACHINE_CLASSES = ["control-plane", "worker"]

# Which class each host of the testbed belongs to. Consumed by the measurement
# harness so that a per-node replay can pick the right prior set.
HOST_CLASSES = {
    "master": "control-plane",
    "node_1": "worker",
    "node_2": "worker",
    "node_3": "worker",
}


def compute_construct_scores_by_class(
    root_dir: str | None = None,
) -> tuple[dict[str, dict[str, dict[str, float]]], dict[str, dict[str, str]]]:
    """
    Returns ({kd: {machine_class: {construct: score}}}, provenance) with scores in
    [0, 1] and provenance recording, per construct and class, whether the score is
    class-specific or inherited from the cross-class figure.
    """
    cross = compute_construct_scores(root_dir)

    throughput_n = _norm(DP_THROUGHPUT_OPS)

    by_class: dict[str, dict[str, dict[str, float]]] = {}
    for kd in KDS:
        by_class[kd] = {}
        for cls in MACHINE_CLASSES:
            scores = dict(cross[kd])  # start from the cross-class figures
            if cls == "worker":
                # Data-plane throughput is measured against processes that run here.
                scores["PS"] = round(throughput_n[kd], 4)
            # The control-plane class inherits every construct: nothing in the
            # campaign measures its local behaviour. Saying so is the point — a
            # uniform number presented as class-specific would be worse than an
            # acknowledged inheritance.
            by_class[kd][cls] = {k: round(v, 4) for k, v in scores.items()}

    provenance = {
        "PS": {
            "control-plane": "inherited: the available performance constants are "
                             "pod-startup latency, timed end to end across client, API "
                             "server, scheduler, kubelet and runtime, and data-plane "
                             "throughput, measured against worker-hosted processes. "
                             "Neither measures the control-plane host's local "
                             "behaviour. Routing latency here was tried and measured "
                             "worse: the master's divergence grew 6.2-9.5% on all nine "
                             "k0s workloads.",
            "worker": "class-specific: data-plane throughput, memtier against Redis "
                      "processes executing on workers (P1). Measured better: the "
                      "worker's divergence shrank 6.3-8.8% on all nine k0s workloads.",
        },
        "RC": {
            "control-plane": "inherited: every quantity feeding RC is worker-measured "
                             "(P4's Cortex-A72 DVFS model, P5's node_2 cgroups). The "
                             "campaign contains no control-plane-side energy "
                             "measurement, so no class-specific score exists.",
            "worker": "class-specific: P4 energy per pod and control-plane power "
                      "overhead, both from the RPi4 model",
        },
        "CO": {
            cls: "cross-class: offline preservation is architectural rather than "
                 "per-host, and interrupt amplification is not resolved per host"
            for cls in MACHINE_CLASSES
        },
    }
    return by_class, provenance


def compute_construct_scores(root_dir: str | None = None) -> dict[str, dict[str, float]]:
    """
    Returns {kd: {construct_id: score}} with all scores in [0, 1].
    """
    energy = load_energy_efficiency(root_dir)
    irq    = load_interrupt_amplification(root_dir)

    # ── PS: Performance & Scalability ─────────────────────────────────────
    latency_inv = _norm_inv(POD_STARTUP_LATENCY_MS)
    throughput_n = _norm(DP_THROUGHPUT_OPS)
    ps = _blend(latency_inv, throughput_n, w_a=0.5)

    # ── SC: Security & Compliance ─────────────────────────────────────────
    sc = _norm(SECURITY_SCORES)

    # ── RR: Reliability & Resilience ──────────────────────────────────────
    recovery_inv = _norm_inv(RECOVERY_TIME_MIN)
    offline_n    = _norm(OFFLINE_PRESERVATION)
    rr = _blend(recovery_inv, offline_n, w_a=0.6)

    # ── MU: Maintainability & Usability ───────────────────────────────────
    mu = _norm_inv(SETUP_TIME_HOURS)

    # ── RC: Resource Constraints & Cost ───────────────────────────────────
    # Control-plane power overhead only. The energy-per-pod figure was dropped: it
    # comes from a DVFS model of one hardware class, and applying it to a system
    # whose energy was never measured produces a number with no referent. What is
    # left is an overhead measurement, which is what the constructs's name claims.
    overhead = {kd: energy[kd]["cp_overhead_w"] or 0.35 for kd in KDS}
    rc = _norm_inv(overhead)

    # ── CO: Connectivity & Offline Resilience ─────────────────────────────
    # offline preservation + inverted interrupt amplification (lower amp = less overhead)
    cp_amp = {kd: irq.get(kd, {}).get("cp_amplification", 2.0) for kd in KDS}
    amp_inv = _norm_inv(cp_amp)
    co = _blend(_norm(OFFLINE_PRESERVATION), amp_inv, w_a=0.7)

    # ── CE: Community & Ecosystem ─────────────────────────────────────────
    ce = _norm(GITHUB_STARS)

    scores: dict[str, dict[str, float]] = {}
    for kd in KDS:
        scores[kd] = {
            "PS": round(ps[kd], 4),
            "SC": round(sc[kd], 4),
            "RR": round(rr[kd], 4),
            "MU": round(mu[kd], 4),
            "RC": round(rc[kd], 4),
            "CO": round(co[kd], 4),
            "CE": round(ce[kd], 4),
        }
    return scores



# Domain-knowledge overrides for propositions where the Spearman proxy is
# known to be inadequate.  Format: {prop_id: (strength, reason)}.
# These replace the proxy-computed λ but the Spearman statistics are still
# reported for transparency.
_OVERRIDES: dict[str, tuple[float, str]] = {
    # P2 and P3 both map RC→PS with opposite polarities (known Di-Select conflict).
    # The single Spearman ρ (+0.70) confirms P3 but inverts P2.  Both propositions
    # capture real mechanisms (throughput overhead vs latency efficiency) that
    # operate at different sub-dimensions of PS.  Assign equal strength so the
    # agent balances both effects symmetrically at cold-start.
    "P2":  (0.55, "P2/P3 conflict: RC→PS has opposing mechanisms (overhead vs efficiency). "
                  "ρ=+0.70 confirms P3 direction. P2 assigned conservative λ=0.55 via "
                  "domain knowledge; kubeEdge throughput penalty (75% lower than k8s) "
                  "provides direct empirical support."),
    # P1: SC→RC proxy is masked because k8s achieves high security AND high
    # energy efficiency (10.18 J/pod, lowest of all KDs), defeating the expected
    # positive correlation.  Domain knowledge from P2 (security overhead): k3s
    # 7.21% security / lowest CPU; k8s 55% / highest baseline overhead.
    "P1":  (0.62, "Proxy masked: k8s high SC + high energy efficiency overrides expected "
                  "SC-overhead pattern. P2 paper evidence: security compliance positively "
                  "correlates with setup/maintenance overhead (P12 ρ=-0.884 confirms). "
                  "λ=0.62 from domain knowledge (security CIS overhead documented in P2)."),
    # P5: RR proxy is recovery-time dominated; offline preservation (P5's mechanism)
    # only contributes 40%.  k3s fast recovery dominates despite low CO score.
    "P5":  (0.65, "RR proxy dominated by recovery-time metric where k3s/k0s excel. "
                  "P5 specifically captures continuity DURING outage — a binary quality. "
                  "kubeEdge/openYurt preserve messages during partition (P2 paper). "
                  "λ=0.65 from domain knowledge; distinct from P6 (cloud-dependency penalty)."),
    # P11: CE→SC near-zero proxy because kubeEdge/openYurt share k8s security score
    # despite small community.  The mechanism (community patches → security) operates
    # on a longer timescale than the benchmark window.
    "P11": (0.48, "Near-zero proxy ρ because kubeEdge/openYurt reach same CIS score as k8s "
                  "via different paths (custom hardening vs upstream patches). "
                  "λ=0.48 from literature; community patch velocity well-documented but "
                  "not observable in single benchmark snapshot."),
}


def compute_proposition_strengths(
    construct_scores: dict[str, dict[str, float]]
) -> dict[str, dict]:
    """
    Returns {prop_id: {prior_strength, direction, from_construct, to_construct,
                        spearman_rho, calibration_note, method}}.

    Method field: 'spearman' for proxy-based estimates, 'domain_override' for
    propositions where proxy adequacy is insufficient.
    """
    results: dict[str, dict] = {}

    for prop_id, from_c, to_c, direction in PROPOSITIONS:
        x = [construct_scores[kd][from_c] for kd in KDS]
        y = [construct_scores[kd][to_c]   for kd in KDS]

        rho, pval = spearmanr(x, y)
        proxy_strength = _spearman_strength(x, y)

        sign_consistent = (direction == "positive" and rho > 0) or \
                          (direction == "negative" and rho < 0)

        if prop_id in _OVERRIDES:
            strength, override_note = _OVERRIDES[prop_id]
            method = "domain_override"
            note = override_note
        else:
            strength = proxy_strength
            method = "spearman"
            note = (
                f"|ρ|={proxy_strength:.3f} (p={pval:.3f}); "
                f"direction {'confirmed' if sign_consistent else 'reversed — proxy limitation'}"
            )

        results[prop_id] = {
            "prior_strength":    round(strength, 4),
            "direction":         direction,
            "from_construct":    from_c,
            "to_construct":      to_c,
            "spearman_rho":      round(float(rho), 4),
            "p_value":           round(float(pval), 4),
            "sign_consistent":   int(sign_consistent),
            "n_distributions":   len(KDS),
            "method":            method,
            "calibration_note":  note,
        }

    return results
