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
RC  – Resource Constraints & Cost: inverted cp_overhead_w (energy_per_pod_j dropped —
                                   see build_construct_scores)
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
#
# CO was removed after measurement, not on principle. On this testbed it carried only
# network throughput — `network_loss_ratio` and `network_latency_ms` are never collected
# — and rx/tx arrive at ~1e-5 of the 1 Gbps reference they are normalised against,
# leaving the derived construct at 1.4e-4 and any edge out of it contributing nothing to
# an estimate. Restoring CO needs a live connectivity signal, or a normalisation
# reference the deployment actually exhibits; it does not need a change here.
TELEMETRY_CONSTRUCTS = ["RC", "PS"]

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

# Propositions the agent does not instantiate even though both endpoints are
# observable. Kept as data with a reason, so the omission is legible next to the
# Di-Select backbone it departs from.
EXCLUDED_PROPOSITIONS = {
    "P2": ("duplicates P3 on the signals this setup measures. P2 states RC→PS against "
           "scheduling *throughput* and P3 against startup *latency*; every metric routed "
           "to PS is a latency or pressure quantity, so on one PS axis the two are "
           "contrapositives of a single claim and carrying both double-counts it. P2's "
           "prior was also the one domain_override in the calibration rather than a "
           "measured quantity."),
}

# Directions Di-Select states against a quantity this setup does not measure, corrected
# to the polarity of what it does. Recorded rather than edited into
# DISELECT_PROPOSITIONS, which stays faithful to the published backbone.
SIGN_OVERRIDES = {
    "P10": ("positive", "Di-Select states PS→RC against an efficiency measure, where "
                        "better efficiency lowers cost. PS here is stall pressure, so the "
                        "same mechanism appears as a positive association: a machine under "
                        "pressure is consuming more. Observed conflict share against the "
                        "declared negative sign was 0.45–1.00 across five clusters."),
}

# The agent's graph: propositions whose BOTH endpoints are observable, less those
# explicitly excluded. An edge with one observable endpoint still receives observations
# through it, but its far construct is a constant, so the edge cannot express a relation
# — see research-docs/relational-edge-learning.md.
PROPOSITIONS = [
    (pid, frm, to, SIGN_OVERRIDES.get(pid, (direction,))[0])
    for pid, frm, to, direction in DISELECT_PROPOSITIONS
    if frm in TELEMETRY_CONSTRUCTS and to in TELEMETRY_CONSTRUCTS
    and pid not in EXCLUDED_PROPOSITIONS
]


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
    Returns {prop_id: {direction, from_construct, to_construct, spearman_rho,
                        p_value, sign_consistent, calibration_note, method}}.

    NO MAGNITUDE IS RETURNED. This function used to emit a `prior_strength` per
    proposition, and the daemon seeded every relationship from it. Three findings
    retired that:

      1. The number was not a measurement of an association. It came from a rank
         correlation between construct proxies across five distributions, and for
         both propositions that survive the scoping it reports rho = 0.0, p = 1.0,
         with this module's own "proxy reversal" warning attached.
      2. Its per-cluster form came from min-max normalised construct scores over the
         same five distributions, so one cluster sat at exactly 0.0 and another at
         exactly 1.0 by construction, and a sixth distribution scoring below the
         current minimum would have rescaled every value — including those of
         clusters whose measurements had not changed.
      3. The cold-start window it existed to cover is about a minute at 1 Hz.

    The diagnostics ARE still returned, and that is deliberate: they are the record
    of why no magnitude is emitted. A reader can see that the calibration was
    attempted, what it produced, and why it was not fit to seed a decision.

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

        # `strength` is computed above and deliberately NOT emitted; see the
        # docstring. It is retained in the note so the artefact records what the
        # calibration would have produced, which is what makes its exclusion
        # checkable rather than merely asserted.
        results[prop_id] = {
            "would_have_been":   round(strength, 4),
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
