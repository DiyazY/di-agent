"""Sensor models for propulsion measurements that the FEEMS electric drive
model does not produce directly: lever position feedback, hull speed, and
propeller shaft torque.

The propulsion drive here is a single electric azipod (rectifier + inverter +
motor), not a mechanical gas-turbine drivetrain. Gas-turbine-specific
quantities (GT/GG shaft speeds and torque, compressor/turbine temperatures and
pressures, turbine injection control, fuel flow, and a port/starboard torque
split) have no physical counterpart in this architecture and no fuel/thermal
model to estimate them from, so they are intentionally not simulated.

The three quantities below are estimated from values the electric drive model
already produces (load ratio, power output, shaft speed) plus a plausible
hull propeller law, each with added zero-mean Gaussian measurement noise.
"""

from dataclasses import dataclass

import numpy as np

# Ship speed at 100% load ratio. There is no hull resistance model in this
# system, so speed is only a rough estimate derived via the typical cubic
# propeller law (power ~ speed^3), anchored to this assumed top speed.
SHIP_MAX_SPEED_KNOTS = 22.0


@dataclass(frozen=True)
class NoisySensor:
    """Wraps a deterministic estimate with zero-mean Gaussian measurement noise."""

    name: str
    std_dev: float

    def sample(self, rng: np.random.Generator, estimate: float) -> float:
        return max(0.0, float(rng.normal(estimate, self.std_dev)))


LEVER_POSITION_SENSOR = NoisySensor("lever_position_pct", std_dev=0.3)
SHIP_SPEED_SENSOR = NoisySensor("ship_speed_knots", std_dev=0.15)
PROPELLER_TORQUE_SENSOR = NoisySensor("propeller_torque_kn_m", std_dev=0.5)

# Anomaly mode simulates hull/propeller fouling or cavitation: the same power
# output yields less thrust (lower ship speed) while the propeller has to work
# harder against the disturbed flow (higher, noisier torque).
_ANOMALY_SHIP_SPEED_MULTIPLIER = 0.8
_ANOMALY_TORQUE_MULTIPLIER = 1.3
_ANOMALY_TORQUE_NOISE_MULTIPLIER = 4.0


class SensorSimulator:
    """Samples propulsion sensors for one telemetry step."""

    def __init__(self, seed: int | None = None) -> None:
        self._rng = np.random.default_rng(seed)

    def simulate(
        self,
        *,
        target_load_ratio: float,
        load_ratio: float,
        power_output_kw: float,
        speed_rpm: float,
        anomaly_enabled: bool = False,
    ) -> dict[str, float]:
        # Lever position is the commanded throttle input (0-100%), not the
        # power-limited achieved load, read back through a noisy potentiometer.
        lever_position_pct = LEVER_POSITION_SENSOR.sample(self._rng, target_load_ratio * 100.0)

        ship_speed_estimate = SHIP_MAX_SPEED_KNOTS * max(load_ratio, 0.0) ** (1.0 / 3.0)
        if anomaly_enabled:
            ship_speed_estimate *= _ANOMALY_SHIP_SPEED_MULTIPLIER
        ship_speed_knots = SHIP_SPEED_SENSOR.sample(self._rng, ship_speed_estimate)

        if speed_rpm > 0:
            shaft_omega_rad_s = speed_rpm * 2 * np.pi / 60.0
            torque_estimate_kn_m = power_output_kw / shaft_omega_rad_s
        else:
            torque_estimate_kn_m = 0.0
        if anomaly_enabled:
            torque_estimate_kn_m *= _ANOMALY_TORQUE_MULTIPLIER
            propeller_torque_kn_m = float(
                self._rng.normal(
                    torque_estimate_kn_m,
                    PROPELLER_TORQUE_SENSOR.std_dev * _ANOMALY_TORQUE_NOISE_MULTIPLIER,
                )
            )
            propeller_torque_kn_m = max(0.0, propeller_torque_kn_m)
        else:
            propeller_torque_kn_m = PROPELLER_TORQUE_SENSOR.sample(self._rng, torque_estimate_kn_m)

        return {
            "lever_position_pct": lever_position_pct,
            "ship_speed_knots": ship_speed_knots,
            "propeller_torque_kn_m": propeller_torque_kn_m,
        }
