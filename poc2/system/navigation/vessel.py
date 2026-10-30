"""Kinematic route-following vessel model for the navigation simulator.

The vessel steams along a waypoint loop at a configurable target speed over
ground. Motion is integrated in a local equirectangular plane (metres
east/north of a reference point), which is accurate enough for lab-scale
routes of a few nautical miles. The model supports time acceleration
(time_scale > 1) so a realistic route loop completes in minutes instead of
hours — distance, speed and heading remain physically consistent, only wall
time is compressed. Wave-induced attitude is intentionally not part of this
model; it is a real-time phenomenon and lives in sensors.py.
"""

from dataclasses import dataclass
from math import atan2, cos, degrees, radians, sin, sqrt

M_PER_DEG_LAT = 111_320.0
M_PER_NM = 1852.0
KNOTS_TO_M_S = M_PER_NM / 3600.0


def _wrap_deg(angle: float) -> float:
    return angle % 360.0


def _shortest_turn_deg(current: float, target: float) -> float:
    """Signed shortest turn from current to target heading, in [-180, 180]."""
    return (target - current + 180.0) % 360.0 - 180.0


@dataclass(frozen=True)
class Waypoint:
    lat: float
    lon: float


# Default route: a ~1.2 nm square loop in the Singapore Strait.
DEFAULT_ROUTE = (
    Waypoint(1.2644, 103.8200),
    Waypoint(1.2644, 103.8412),
    Waypoint(1.2856, 103.8412),
    Waypoint(1.2856, 103.8200),
)


class VesselModel:
    """Route follower with turn-rate-limited heading and a synthetic seabed."""

    # Turn rate limit (deg/s of simulated time) — a gentle vessel-like turn.
    MAX_TURN_DEG_PER_S = 4.0
    # Distance to a waypoint at which the leg is considered complete.
    ARRIVAL_RADIUS_M = 150.0

    def __init__(
        self,
        route: tuple[Waypoint, ...] = DEFAULT_ROUTE,
        speed_knots: float = 8.0,
        time_scale: float = 60.0,
    ) -> None:
        self._ref_lat = route[0].lat
        self._ref_lon = route[0].lon
        self._m_per_deg_lon = M_PER_DEG_LAT * cos(radians(self._ref_lat))
        self._route: list[tuple[float, float]] = [self._to_xy(wp) for wp in route]
        self._speed_m_s = speed_knots * KNOTS_TO_M_S
        self._time_scale = time_scale

        self._x, self._y = self._route[0]
        self._heading_deg = self._bearing_to(self._route[1])
        self._rot_deg_per_min = 0.0
        self._waypoint_index = 1
        self._distance_m = 0.0  # along-track distance, drives the seabed profile
        self._total_log_m = 0.0
        self._trip_log_m = 0.0
        self._sim_time_s = 0.0

    def set_speed_knots(self, speed_knots: float) -> None:
        if not 0.0 <= speed_knots <= 30.0:
            raise ValueError("speed_knots must be between 0 and 30")
        self._speed_m_s = speed_knots * KNOTS_TO_M_S

    def set_route(self, waypoints: list[Waypoint]) -> None:
        if len(waypoints) < 2:
            raise ValueError("route needs at least 2 waypoints")
        self._route = [self._to_xy(wp) for wp in waypoints]
        self._waypoint_index = 0
        self._trip_log_m = 0.0

    def step(self, dt_wall_s: float) -> None:
        """Advance the model by dt_wall_s seconds of wall (real) time."""
        dt = dt_wall_s * self._time_scale
        self._sim_time_s += dt
        target = self._route[self._waypoint_index]

        desired = self._bearing_to(target)
        turn = _shortest_turn_deg(self._heading_deg, desired)
        max_turn = self.MAX_TURN_DEG_PER_S * dt
        applied = max(-max_turn, min(max_turn, turn))
        self._heading_deg = _wrap_deg(self._heading_deg + applied)
        self._rot_deg_per_min = (applied / dt) * 60.0 if dt > 0 else 0.0

        step_m = self._speed_m_s * dt
        self._x += sin(radians(self._heading_deg)) * step_m
        self._y += cos(radians(self._heading_deg)) * step_m
        self._distance_m += step_m
        self._total_log_m += step_m
        self._trip_log_m += step_m

        if self._distance_to(target) < self.ARRIVAL_RADIUS_M:
            self._waypoint_index = (self._waypoint_index + 1) % len(self._route)

    def snapshot(self) -> dict:
        """Ground-truth navigation state (what perfect sensors would read)."""
        lat, lon = self._to_lat_lon(self._x, self._y)
        return {
            "latitude_deg": lat,
            "longitude_deg": lon,
            # No current/leeway in the model: course over ground equals heading.
            "heading_deg": self._heading_deg,
            "cog_deg": self._heading_deg,
            "sog_knots": self._speed_m_s / KNOTS_TO_M_S,
            "rot_deg_per_min": self._rot_deg_per_min,
            "depth_m": self._depth_at(self._distance_m),
            "total_log_m": self._total_log_m,
            "trip_log_m": self._trip_log_m,
            "sim_time_s": self._sim_time_s,
            "waypoint_index": self._waypoint_index,
            "speed_target_knots": self._speed_m_s / KNOTS_TO_M_S,
            "route": [
                {"lat": lat, "lon": lon}
                for lat, lon in (self._to_lat_lon(x, y) for x, y in self._route)
            ],
        }

    def _depth_at(self, distance_m: float) -> float:
        """Synthetic seabed: a repeatable function of along-track distance,
        so the depth sounder traces the same profile every loop."""
        return 40.0 + 18.0 * sin(distance_m / 600.0) + 8.0 * sin(distance_m / 173.0)

    def _to_xy(self, wp: Waypoint) -> tuple[float, float]:
        return (
            (wp.lon - self._ref_lon) * self._m_per_deg_lon,
            (wp.lat - self._ref_lat) * M_PER_DEG_LAT,
        )

    def _to_lat_lon(self, x: float, y: float) -> tuple[float, float]:
        return (
            self._ref_lat + y / M_PER_DEG_LAT,
            self._ref_lon + x / self._m_per_deg_lon,
        )

    def _distance_to(self, target: tuple[float, float]) -> float:
        return sqrt((target[0] - self._x) ** 2 + (target[1] - self._y) ** 2)

    def _bearing_to(self, target: tuple[float, float]) -> float:
        return _wrap_deg(degrees(atan2(target[0] - self._x, target[1] - self._y)))
