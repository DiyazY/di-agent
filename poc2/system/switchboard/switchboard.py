from dataclasses import dataclass


@dataclass
class ConsumerRequest:
    consumer_id: str
    requested_power_kw: float
    priority: int
    received_at: float


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
