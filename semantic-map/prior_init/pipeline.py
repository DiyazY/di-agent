"""
Prior initialization pipeline entry point.

Usage:
    python -m semantic-map.prior_init.pipeline [--root ROOT] [--out OUT]

    --root  Path to the mega-research repo root (default: auto-detected)
    --out   Output path for prior_weights.json (default: semantic-map/prior_weights.json)

Outputs prior_weights.json with:
  - version + metadata
  - propositions: calibrated PriorStrength λ per proposition
  - distribution_construct_scores: normalised [0,1] construct scores per KD
  - distribution_edge_weights: per-distribution edge priors for storage seeding
"""

from __future__ import annotations

import argparse
import json
import sys
from datetime import date
from pathlib import Path

from .calibration import (compute_construct_scores, compute_construct_scores_by_class,
                          compute_proposition_strengths, PROPOSITIONS, KDS,
                          TELEMETRY_CONSTRUCTS, CONSTRUCT_META, DISELECT_PROPOSITIONS,
                          MACHINE_CLASSES, HOST_CLASSES)


# TELEMETRY_CONSTRUCTS is imported from calibration, which is the single source
# of the domain model's scope. It was previously duplicated here as a set literal,
# which shadowed the import and broke JSON serialization.

# Bounds on an emitted edge prior. These match the operator tuner's global bounds
# in go/pkg/semmap/map.go (floor 0.10, ceiling 0.95) deliberately: the pipeline
# must not emit a prior that the tuner would refuse to let an operator set. They
# also keep the prior off the boundary where the Bernoulli KL divergence used in
# the convergence study is unbounded — a prior of exactly 0 makes any nonzero
# observation register as near-infinite information gain.
EDGE_PRIOR_FLOOR = 0.10
EDGE_PRIOR_CEIL = 0.95


def build_edge_weights(
    construct_scores: dict[str, dict[str, float]],
    proposition_strengths: dict[str, dict],
) -> dict[str, dict[str, dict]]:
    """
    Build per-distribution edge descriptors for storage seeding.

    For each (distribution, proposition) pair, produce an edge descriptor:
      from_id        – source construct ID
      to_id          – target construct ID
      proposition_id – P1..P15
      direction      – positive | negative
      prior_weight   – see below
      ema_weight     – same as prior_weight at T=0 (no observations yet)

    Per-distribution calibration is restricted to edges whose SOURCE construct
    has an instrumented runtime-telemetry proxy (PS, RC, CO). For those, the
    prior is the distribution's construct score modulated by the proposition
    strength, as before.

    Edges sourced from SC, MU or CE fall back to the global proposition strength
    with no per-distribution modulation. Their proxies do not survive scrutiny as
    empirical calibration inputs: SC is a CIS score produced by a third-party
    scanner, CE is a GitHub star count, and MU is setup time measured in human
    hours. None is a system measurement, and min-max normalising three such
    proxies across five distributions produced magnitudes that read as empirical
    but encoded only a rank within the sample. Falling back to the global lambda
    states the honest position: those constructs carry Di-Select's structural
    prior and nothing distribution-specific.

    This also makes the treatment consistent end to end. SC, MU, CE and RR have
    no runtime telemetry analog, so they never accumulate evidence (paper §5.2);
    they are now equally uncalibrated per-distribution at initialization. They
    are structural priors at every stage, revisable only by explicit operator
    action.

    Every emitted prior is finally clamped to [EDGE_PRIOR_FLOOR,
    EDGE_PRIOR_CEIL]. The clamp is applied to the product, not to the construct
    score, because multiplying a floored score by a strength below 1.0 drags it
    back under the floor.
    """
    edges: dict[str, dict[str, dict]] = {}

    for kd in KDS:
        edges[kd] = {}
        for prop_id, from_c, to_c, direction in PROPOSITIONS:
            strength = proposition_strengths[prop_id]["prior_weight"] = \
                       proposition_strengths[prop_id]["prior_strength"]

            if from_c in TELEMETRY_CONSTRUCTS:
                raw = construct_scores[kd][from_c] * strength
            else:
                raw = strength

            prior = round(min(EDGE_PRIOR_CEIL, max(EDGE_PRIOR_FLOOR, raw)), 4)

            edge_key = f"{from_c}→{to_c}:{prop_id}"
            edges[kd][edge_key] = {
                "from_id":        from_c,
                "to_id":          to_c,
                "proposition_id": prop_id,
                "direction":      direction,
                "prior_weight":   prior,
                "ema_weight":     prior,   # identical at cold-start
                "calibrated":     from_c in TELEMETRY_CONSTRUCTS,
            }

    return edges


def build_machine_class_edge_weights(
    scores_by_class: dict[str, dict[str, dict[str, float]]],
    proposition_strengths: dict[str, dict],
) -> dict[str, dict[str, dict[str, dict]]]:
    """
    Build {kd: {machine_class: {edge_key: descriptor}}}.

    Identical arithmetic to build_edge_weights, applied to the per-class construct
    scores. A cluster's machines are not interchangeable — on this testbed an x86
    control-plane host and ARM workers — and calibrating them alike misattributes
    every measurement that was taken on one class to all of them.
    """
    out: dict[str, dict[str, dict[str, dict]]] = {}
    for kd in KDS:
        out[kd] = {}
        for cls in MACHINE_CLASSES:
            out[kd][cls] = {}
            for prop_id, from_c, to_c, direction in PROPOSITIONS:
                strength = proposition_strengths[prop_id]["prior_strength"]
                if from_c in TELEMETRY_CONSTRUCTS:
                    raw = scores_by_class[kd][cls][from_c] * strength
                else:
                    raw = strength
                prior = round(min(EDGE_PRIOR_CEIL, max(EDGE_PRIOR_FLOOR, raw)), 4)
                out[kd][cls][f"{from_c}→{to_c}:{prop_id}"] = {
                    "from_id":        from_c,
                    "to_id":          to_c,
                    "proposition_id": prop_id,
                    "direction":      direction,
                    "prior_weight":   prior,
                    "ema_weight":     prior,
                    "calibrated":     from_c in TELEMETRY_CONSTRUCTS,
                }
    return out


def run(root_dir: str | None = None, out_path: str | None = None) -> dict:
    """Execute the pipeline and return the output document."""
    construct_scores  = compute_construct_scores(root_dir)
    prop_strengths    = compute_proposition_strengths(construct_scores)
    edge_weights      = build_edge_weights(construct_scores, prop_strengths)
    scores_by_class, class_provenance = compute_construct_scores_by_class(root_dir)
    class_edge_weights = build_machine_class_edge_weights(scores_by_class, prop_strengths)

    # Summarise reversed propositions.  Overridden ones are noted separately.
    warnings = []
    for pid, v in prop_strengths.items():
        if not v["sign_consistent"]:
            tag = "(domain_override)" if v["method"] == "domain_override" else "(proxy reversal — investigate)"
            warnings.append(f"{pid} {tag}: ρ={v['spearman_rho']:.3f}")

    # The constructs block makes the agent's domain model a data artifact. The Go
    # ontology reads constructs and propositions from this file rather than
    # carrying them as literals, so reshaping the graph — adding a construct once
    # a MetricType routes to it, retiring one that turns out not to be runtime
    # state — is a regeneration, not a recompile.
    constructs = [
        {"construct_id": cid,
         "name":         CONSTRUCT_META[cid][0],
         "description":  CONSTRUCT_META[cid][1]}
        for cid in TELEMETRY_CONSTRUCTS
    ]

    output = {
        "version":        "2.0",
        "generated_at":   str(date.today()),
        "evidence_papers": ["P1", "P2", "P4", "P5"],
        "distributions":  KDS,
        "warnings":       warnings,
        "scope": {
            "telemetry_constructs": TELEMETRY_CONSTRUCTS,
            "diselect_propositions": len(DISELECT_PROPOSITIONS),
            "agent_propositions":   [p[0] for p in PROPOSITIONS],
            "rationale": (
                "The agent's graph carries only propositions whose both endpoints "
                "are observable at runtime. An edge with no observable endpoint is "
                "inert in the Reasoner: cost accumulates (effective - prior), and "
                "an unobserved edge has effective == prior, so it contributes "
                "exactly zero regardless of its prior or of operator action. "
                "Di-Select remains the origin of the causal claims; the agent "
                "instantiates the subset it can observe."
            ),
        },
        "constructs":     constructs,
        "propositions":   prop_strengths,
        "distribution_construct_scores":  construct_scores,
        "distribution_edge_weights":      edge_weights,
        "machine_classes": {
            "rationale": (
                "A cluster's machines are not interchangeable, and calibrating them "
                "alike misattributes measurements. On this testbed the control-plane "
                "host is an x86 NUC and the workers are ARM RPi4s. Each source "
                "constant is routed to the class it was actually measured on; where a "
                "class has no measurement the score is inherited from the cross-class "
                "figure and said to be inherited, so the gap is visible rather than "
                "hidden behind a uniform prior. Nothing is re-derived from the "
                "telemetry the agent later learns from, which would make the prior a "
                "summary of the evaluation data."
            ),
            "classes":     MACHINE_CLASSES,
            "host_classes": HOST_CLASSES,
            "provenance":  class_provenance,
        },
        "machine_class_construct_scores": scores_by_class,
        "machine_class_edge_weights":     class_edge_weights,
    }

    # Write output
    repo_root = Path(__file__).resolve().parents[2]
    default_out = repo_root / "semantic-map" / "prior_weights.json"
    target = Path(out_path) if out_path else default_out
    target.parent.mkdir(parents=True, exist_ok=True)
    with open(target, "w") as f:
        json.dump(output, f, indent=2)
    print(f"Wrote {target}")

    if warnings:
        print(f"\nWARNINGS ({len(warnings)}):")
        for w in warnings:
            print(f"  ⚠  {w}")

    return output


def main() -> None:
    parser = argparse.ArgumentParser(description="Semantic Map prior initialization pipeline")
    parser.add_argument("--root", default=None, help="Repo root directory")
    parser.add_argument("--out",  default=None, help="Output path for prior_weights.json")
    args = parser.parse_args()
    run(root_dir=args.root, out_path=args.out)


if __name__ == "__main__":
    main()
