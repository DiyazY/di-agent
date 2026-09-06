"""Gaussian sensor models for auxiliary genset measurements that the FEEMS
physics model does not produce (ambient conditions, lube oil, vibration, and
per-cylinder values).

Each sensor is sampled once per telemetry step with a mean that varies linearly
with the genset load ratio, plus zero-mean Gaussian noise. Per-cylinder sensors
additionally draw a fixed bias per cylinder at startup so each cylinder keeps a
distinct, persistent baseline (injector/timing spread).
"""

from dataclasses import dataclass

import numpy as np

NUM_CYLINDERS = 8  # Wartsila 8V31DF


@dataclass(frozen=True)
class ScalarSensor:
    """Scalar Gaussian sensor whose mean varies linearly with load ratio."""

    name: str
    mean_at_zero_load: float
    mean_at_full_load: float
    std_dev: float

    def sample(self, rng: np.random.Generator, load_ratio: float) -> float:
        mean = self.mean_at_zero_load + (
            self.mean_at_full_load - self.mean_at_zero_load
        ) * load_ratio
        return max(0.0, float(rng.normal(mean, self.std_dev)))


SCALAR_SENSORS = (
    # Engine-room intake air pressure, effectively load-independent.
    ScalarSensor("air_pressure_kpa", 101.3, 101.3, 0.15),
    # Engine-room ambient temperature; PDF max without derating is 45 C.
    ScalarSensor("ambient_temp_c", 30.0, 38.0, 0.5),
    # Lube oil before bearings: nominal 75 C, alarm 85 C (PDF, TE201/PT201).
    # Thermostat-controlled, so only a slight rise with load.
    ScalarSensor("oil_temp_c", 70.0, 76.0, 0.5),
    ScalarSensor("oil_pressure_bar", 3.9, 4.3, 0.06),
    # Vibration velocity (mm/s RMS) per axis, grows with speed and load.
    ScalarSensor("vibration_x_mm_s", 1.1, 3.6, 0.25),
    ScalarSensor("vibration_y_mm_s", 1.2, 3.8, 0.25),
    ScalarSensor("vibration_z_mm_s", 1.4, 4.4, 0.30),
)


@dataclass(frozen=True)
class CylinderSensor:
    """Per-cylinder Gaussian sensor with a fixed per-cylinder baseline bias.

    load_curve holds (load_ratio, mean) breakpoints, piecewise-linearly
    interpolated, so non-monotonic behaviour (e.g. gas-mode exhaust temperature
    peaking at 50% load) can be reproduced.
    """

    name: str
    load_curve: tuple[tuple[float, float], ...]
    std_dev: float
    biases: tuple[float, ...]

    def mean_at(self, load_ratio: float) -> float:
        loads, means = zip(*self.load_curve)
        return float(np.interp(load_ratio, loads, means))

    def sample(self, rng: np.random.Generator, load_ratio: float) -> np.ndarray:
        values = rng.normal(self.mean_at(load_ratio) + np.asarray(self.biases), self.std_dev)
        return np.maximum(values, 0.0)


def _build_cylinder_sensors(rng: np.random.Generator) -> tuple[CylinderSensor, ...]:
    return (
        # Peak firing pressure per cylinder (not in the PDF; typical for BMEP 2.96 MPa).
        CylinderSensor(
            "cylinder_pressure_bar",
            ((0.0, 115.0), (1.0, 175.0)),
            1.5,
            tuple(rng.normal(0.0, 2.5, NUM_CYLINDERS)),
        ),
        # Exhaust gas temperature per cylinder. Anchored to the PDF gas-mode values
        # after the turbocharger (TE517): 390 C @50%, 360 @75%, 350 @85%, 320 @100%;
        # per-cylinder port temperatures run ~30 C hotter than after the turbocharger.
        # The curve is non-monotonic: hottest at part load in gas mode.
        CylinderSensor(
            "cylinder_exhaust_temp_c",
            ((0.0, 250.0), (0.5, 420.0), (0.75, 390.0), (0.85, 380.0), (1.0, 350.0)),
            4.0,
            tuple(rng.normal(0.0, 6.0, NUM_CYLINDERS)),
        ),
    )


class SensorSimulator:
    """Samples all Gaussian sensors for one telemetry step."""

    def __init__(self, seed: int | None = None) -> None:
        self._rng = np.random.default_rng(seed)
        self._cylinder_sensors = _build_cylinder_sensors(self._rng)

    def simulate(self, load_ratio: float) -> dict[str, float]:
        values = {
            sensor.name: sensor.sample(self._rng, load_ratio) for sensor in SCALAR_SENSORS
        }
        for sensor in self._cylinder_sensors:
            for index, value in enumerate(sensor.sample(self._rng, load_ratio), start=1):
                values[f"{sensor.name}_{index}"] = float(value)
        return values
