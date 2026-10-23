"""Gaussian sensor models that turn vessel ground truth into measured values.

Each sensor adds zero-mean Gaussian noise on top of the vessel model's truth,
like the ScalarSensor pattern in the genset simulator. Environment processes
that must look continuous to a 5 Hz sampler (wave-induced roll/pitch, true
wind) are driven by wall-clock time, not the vessel model's accelerated
simulation time.

Anomaly mode simulates GNSS degradation (growing position offset plus a noise
spike, e.g. spoofing/drifting fix) while heading/speed keep reporting sane
values — a discrepancy that is observable downstream in InfluxDB.
"""

import time
from math import atan2, cos, degrees, pi, radians, sin

import numpy as np

KNOTS_TO_M_S = 1852.0 / 3600.0


class GnssSensor:
    """Position/COG/SOG receiver with ~2.5 m position noise."""

    NOISE_LATLON_M = 2.5
    NOISE_COG_DEG = 0.8
    NOISE_SOG_KNOTS = 0.15
    # Anomaly: position fix drifts north at this rate (m/s of wall time).
    ANOMALY_DRIFT_M_PER_S = 3.0
    ANOMALY_NOISE_MULT = 6.0

    M_PER_DEG_LAT = 111_320.0

    def __init__(self, rng: np.random.Generator, ref_lat: float) -> None:
        self._rng = rng
        self._m_per_deg_lon = self.M_PER_DEG_LAT * cos(radians(ref_lat))
        self._drift_north_m = 0.0

    def sample(self, truth: dict, anomaly: bool, dt_wall_s: float) -> dict:
        noise_mult = self.ANOMALY_NOISE_MULT if anomaly else 1.0
        if anomaly:
            self._drift_north_m += self.ANOMALY_DRIFT_M_PER_S * dt_wall_s
        else:
            self._drift_north_m = 0.0
        noise_north = self._rng.normal(0.0, self.NOISE_LATLON_M * noise_mult)
        noise_east = self._rng.normal(0.0, self.NOISE_LATLON_M * noise_mult)
        return {
            "latitude_deg": truth["latitude_deg"]
            + (noise_north + self._drift_north_m) / self.M_PER_DEG_LAT,
            "longitude_deg": truth["longitude_deg"] + noise_east / self._m_per_deg_lon,
            "cog_deg": (truth["cog_deg"] + self._rng.normal(0.0, self.NOISE_COG_DEG)) % 360.0,
            "sog_knots": max(
                0.0, truth["sog_knots"] + self._rng.normal(0.0, self.NOISE_SOG_KNOTS)
            ),
        }


class CompassSensor:
    """Magnetic compass: heading plus rate of turn."""

    NOISE_HEADING_DEG = 0.5
    NOISE_ROT_DEG_PER_MIN = 0.4

    def __init__(self, rng: np.random.Generator) -> None:
        self._rng = rng

    def sample(self, truth: dict) -> dict:
        return {
            "heading_deg": (truth["heading_deg"] + self._rng.normal(0.0, self.NOISE_HEADING_DEG))
            % 360.0,
            "rot_deg_per_min": truth["rot_deg_per_min"]
            + self._rng.normal(0.0, self.NOISE_ROT_DEG_PER_MIN),
        }


class AttitudeSensor:
    """IMU attitude: yaw follows heading; pitch/roll are wave-driven
    sinusoids of wall-clock time so a 5 Hz sampler sees smooth motion."""

    ROLL_AMPLITUDE_DEG = 3.5
    ROLL_PERIOD_S = 9.0
    PITCH_AMPLITUDE_DEG = 1.2
    PITCH_PERIOD_S = 5.5
    NOISE_ATTITUDE_DEG = 0.15

    def __init__(self, rng: np.random.Generator) -> None:
        self._rng = rng
        self._roll_phase = float(rng.uniform(0.0, 2 * pi))
        self._pitch_phase = float(rng.uniform(0.0, 2 * pi))

    def sample(self, truth: dict) -> dict:
        t = time.monotonic()
        roll = self.ROLL_AMPLITUDE_DEG * sin(2 * pi * t / self.ROLL_PERIOD_S + self._roll_phase)
        pitch = self.PITCH_AMPLITUDE_DEG * sin(
            2 * pi * t / self.PITCH_PERIOD_S + self._pitch_phase
        )
        noise = lambda: self._rng.normal(0.0, self.NOISE_ATTITUDE_DEG)  # noqa: E731
        return {
            "yaw_deg": (truth["heading_deg"] + noise()) % 360.0,
            "pitch_deg": pitch + noise(),
            "roll_deg": roll + noise(),
        }


class WindSensor:
    """Apparent wind: slow-varying true wind minus the vessel's motion."""

    TRUE_WIND_BASE_M_S = 8.0
    TRUE_WIND_VAR_M_S = 2.0
    TRUE_WIND_SPEED_PERIOD_S = 240.0
    TRUE_WIND_DIR_BASE_DEG = 220.0
    TRUE_WIND_DIR_VAR_DEG = 25.0
    TRUE_WIND_DIR_PERIOD_S = 600.0
    NOISE_SPEED_M_S = 0.3
    NOISE_ANGLE_DEG = 2.0

    def __init__(self, rng: np.random.Generator) -> None:
        self._rng = rng

    def sample(self, truth: dict) -> dict:
        t = time.monotonic()
        true_speed = self.TRUE_WIND_BASE_M_S + self.TRUE_WIND_VAR_M_S * sin(
            2 * pi * t / self.TRUE_WIND_SPEED_PERIOD_S
        )
        true_dir_from = self.TRUE_WIND_DIR_BASE_DEG + self.TRUE_WIND_DIR_VAR_DEG * sin(
            2 * pi * t / self.TRUE_WIND_DIR_PERIOD_S
        )
        # True wind velocity vector (direction the air moves TOWARD).
        true_to = radians(true_dir_from) + pi
        air_east = true_speed * sin(true_to)
        air_north = true_speed * cos(true_to)
        # Vessel velocity vector; apparent wind = air velocity - vessel velocity.
        vessel_m_s = truth["sog_knots"] * KNOTS_TO_M_S
        vessel_east = vessel_m_s * sin(radians(truth["heading_deg"]))
        vessel_north = vessel_m_s * cos(radians(truth["heading_deg"]))
        app_east = air_east - vessel_east
        app_north = air_north - vessel_north
        app_speed = max(0.0, (app_east**2 + app_north**2) ** 0.5)
        # Angle the apparent wind comes FROM, relative to the bow (0 = ahead,
        # positive to starboard).
        app_from_global = degrees(atan2(-app_east, -app_north)) % 360.0
        app_from_rel = (app_from_global - truth["heading_deg"] + 180.0) % 360.0 - 180.0
        return {
            "wind_speed_m_s": app_speed + self._rng.normal(0.0, self.NOISE_SPEED_M_S),
            # NMEA 2000 wind angle is 0..360 deg clockwise from the bow.
            "wind_angle_deg": app_from_rel % 360.0 + self._rng.normal(0.0, self.NOISE_ANGLE_DEG),
            "reference": "apparent",
            "true_wind_speed_m_s": true_speed,
            "true_wind_direction_deg": true_dir_from,
        }


class DepthSensor:
    """Depth sounder below the transducer, with a fixed mounting offset."""

    NOISE_DEPTH_M = 0.25
    OFFSET_M = 2.0  # transducer sits 2 m below the waterline

    def __init__(self, rng: np.random.Generator) -> None:
        self._rng = rng

    def sample(self, truth: dict) -> dict:
        return {
            "depth_m": max(0.5, truth["depth_m"] + self._rng.normal(0.0, self.NOISE_DEPTH_M)),
            "offset_m": self.OFFSET_M,
        }
