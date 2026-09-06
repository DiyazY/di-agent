from dataclasses import dataclass


@dataclass
class ConsumerRequest:
    consumer_id: str
    requested_power_kw: float
    priority: int
    received_at: float


def _summarize_section(entries: dict[str, dict[str, float | bool]]) -> dict[str, int | float]:
    total = len(entries)
    stale = sum(1 for values in entries.values() if bool(values.get("stale", False)))
    return {"total": total, "stale": stale, "healthy": total - stale}


def summarize_source_health(
    gensets: dict[str, dict[str, float | bool]],
    batteries: dict[str, dict[str, float | bool]],
    consumers: dict[str, dict[str, float | bool]],
) -> dict[str, dict[str, int | float]]:
    """Summarize the health of power sources and consumers in the live data plane.

    This gives the operator a compact view of how many sources are stale or healthy without
    requiring them to inspect every raw telemetry record.
    """
    return {
        "gensets": _summarize_section(gensets),
        "batteries": _summarize_section(batteries),
        "consumers": _summarize_section(consumers),
    }


def allocate_power(
    available_supply_kw: float,
    requests: list[ConsumerRequest],
) -> dict[str, float]:
    """Priority-based load allocation: consumers are served in descending
    priority order (ties broken by consumer_id for determinism) until the
    available bus supply is exhausted. This mirrors how a real switchboard /
    power management system sheds low-priority loads under a power deficit
    instead of letting every consumer draw whatever it wants."""
    remaining = max(available_supply_kw, 0.0)
    allocations: dict[str, float] = {}
    ordered = sorted(requests, key=lambda r: (-r.priority, r.consumer_id))
    for request in ordered:
        granted = max(0.0, min(request.requested_power_kw, remaining))
        allocations[request.consumer_id] = granted
        remaining -= granted
    return allocations
