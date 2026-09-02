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

from .calibration import (compute_construct_scores, compute_proposition_strengths,
                          PROPOSITIONS, KDS, TELEMETRY_CONSTRUCTS, CONSTRUCT_META,
                          DISELECT_PROPOSITIONS, EXCLUDED_PROPOSITIONS,
                          SIGN_OVERRIDES)


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


def build_agent_edges() -> list[dict]:
    """
    Emit the STRUCTURE of each relationship the agent instantiates, and no magnitude.

    For each proposition in scope, one descriptor:
      from_id        – source construct ID
      to_id          – target construct ID
      proposition_id – the label, which is what lets two mechanisms relate one pair
      direction      – positive | negative

    This replaced a per-distribution `distribution_edge_weights` block that carried a
    `prior_weight` and an `ema_weight` for every (distribution, proposition) pair. Three
    reasons it went, and the third is the one that makes this a correctness fix rather
    than a simplification:

      1. The magnitudes were rank-derived, not measured — see
         `calibration.compute_proposition_strengths` for the full account.
      2. The daemon stopped reading them. `profiles.seedStateMap` declares structure and
         leaves every relationship reporting `unknown` until this machine measures it.
      3. So the block was generated, committed, parsed into a Go struct, and never read
         by anything — which is precisely the failure mode this project treats as worse
         than a missing number, because nothing about a number on a page looks wrong.

    The structure is the same for every distribution, because which constructs relate and
    in which direction is a property of the domain rather than of a cluster. Emitting it
    once instead of five times says that, where the per-KD table implied the opposite.
    """
    return [
        {
            "from_id":        from_c,
            "to_id":          to_c,
            "proposition_id": prop_id,
            "direction":      direction,
        }
        for prop_id, from_c, to_c, direction in PROPOSITIONS
    ]


def run(root_dir: str | None = None, out_path: str | None = None) -> dict:
    """Execute the pipeline and return the output document."""
    construct_scores  = compute_construct_scores(root_dir)
    prop_strengths    = compute_proposition_strengths(construct_scores)
    agent_edges       = build_agent_edges()

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
            "excluded_propositions": {
                pid: reason for pid, reason in EXCLUDED_PROPOSITIONS.items()
            },
            "sign_overrides": {
                pid: {"direction": d, "reason": why}
                for pid, (d, why) in SIGN_OVERRIDES.items()
            },
            "rationale": (
                "The agent's graph carries only propositions whose both endpoints this "
                "deployment measures, less those listed in excluded_propositions, and "
                "only where the telemetry does not contradict the declared direction. "
                "A relationship with no observable endpoint is inert in the Reasoner: "
                "it reports 'unknown', has no effective value, and is absent from the "
                "sensitivity sum rather than contributing a term of zero. This artefact "
                "supplies STRUCTURE ONLY -- no strength for any relationship -- because "
                "which constructs relate and in which direction is knowledge one "
                "machine's telemetry cannot produce, while what a relation is worth is "
                "a measurement that only this machine is entitled to make. Di-Select "
                "remains the origin of the causal claims; sign_overrides records where "
                "Di-Select states a direction against a quantity this deployment does "
                "not measure."
            ),
        },
        "constructs":     constructs,
        # Diagnostics per proposition, with no strength among them. `would_have_been`
        # records what the retired calibration would have emitted, so its exclusion is
        # checkable from the artefact rather than only asserted in prose.
        "propositions":   prop_strengths,
        # Restricted to the constructs in scope. Emitting scores for the other four
        # published a number per construct the agent does not carry, several of them
        # min-max proxies over five distributions, which read as measurements of this
        # cluster and were not.
        "distribution_construct_scores": {
            kd: {cid: score for cid, score in scores.items()
                 if cid in TELEMETRY_CONSTRUCTS}
            for kd, scores in construct_scores.items()
        },
        # Structure only. The per-distribution edge-weight table this replaced carried
        # magnitudes that nothing read; see build_agent_edges.
        "agent_edges":    agent_edges,
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
