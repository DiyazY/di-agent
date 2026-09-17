"""Electrical/thermal sensor model for battery measurements that the FEEMS
energy-only Battery model does not produce.

FEEMS tracks only SoC and kW power flow, so terminal voltage, current and
temperature are derived here from a generic LFP-chemistry open-circuit-voltage
(OCV) curve, a lumped internal resistance (IR drop), and a first-order lumped
thermal model (I^2R heating, Newtonian cooling to ambient). Charger/load-side
current and voltage add a small cable-resistance offset from the terminal
values, mirroring a separate instrument on that side of the contactor.
Every measurement layers zero-mean Gaussian noise on top of its physical
estimate, matching the sensor style used in ../genset/sensors.py.

These are deliberately coarse approximations (no manufacturer discharge
curves or thermal specs are available from FEEMS) but are physically
reasonable enough to drive a believable simulation.
"""

from dataclasses import dataclass, field

import numpy as np

# Open-circuit voltage curve for a generic LFP cell, expressed as a fraction of
# nominal_voltage_v vs. SoC. Anchored so 50% SoC == nominal voltage, matching
# LFP's characteristically flat mid-range plateau with steep knees at the ends.
_OCV_SOC_BREAKPOINTS = (0.0, 0.05, 0.2, 0.5, 0.8, 0.95, 1.0)
_OCV_VOLTAGE_FRACTION = (0.875, 0.94, 0.975, 1.0, 1.015, 1.03, 1.08)

# Fraction of nominal_voltage_v the terminal is allowed to sag at max discharge
# current; used to size the pack's lumped internal resistance.
_MAX_VOLTAGE_SAG_FRACTION = 0.08
# Steady-state temperature rise (deg C) above ambient at max continuous discharge;
# used to size the lumped thermal resistance to ambient.
_MAX_TEMP_RISE_C = 25.0
# Thermal mass assumption: J/K per kWh of rated capacity (cells + packaging).
_THERMAL_MASS_J_PER_K_PER_KWH = 6000.0
AMBIENT_TEMP_C = 25.0

# Cable/contactor resistance between the pack terminal and the charger/load-side
# instrumentation, as a fraction of the pack's own internal resistance.
_CABLE_RESISTANCE_FRACTION = 0.1
# Below this power (kW) a charge/discharge path is considered inactive, so a
# cycle boundary (and Time/Capacity reset) can be detected.
_CYCLE_ACTIVE_POWER_KW = 0.05

# Anomaly mode simulates a degraded/faulty cell group: internal resistance rises
# (more voltage sag and heating for the same current) and the noise floor widens
# on every channel, mimicking a loose connection / failing cell group.
_ANOMALY_RESISTANCE_MULTIPLIER = 4.0
_ANOMALY_NOISE_MULTIPLIER = 6.0


@dataclass(frozen=True)
class BatteryElectricalModel:
    """Sizes an internal resistance and lumped thermal parameters from a
    FEEMS Battery's rated capacity and max discharge power, given an assumed
    nominal DC bus voltage."""

    nominal_voltage_v: float
    internal_resistance_ohm: float
    thermal_resistance_k_per_w: float
    thermal_mass_j_per_k: float

    @classmethod
    def from_battery(cls, battery, nominal_voltage_v: float) -> "BatteryElectricalModel":
        max_discharge_current_a = battery.max_discharging_power_kw * 1000 / nominal_voltage_v
        internal_resistance_ohm = (
            _MAX_VOLTAGE_SAG_FRACTION * nominal_voltage_v / max_discharge_current_a
        )
        max_heat_w = max_discharge_current_a**2 * internal_resistance_ohm
        return cls(
            nominal_voltage_v=nominal_voltage_v,
            internal_resistance_ohm=internal_resistance_ohm,
            thermal_resistance_k_per_w=_MAX_TEMP_RISE_C / max_heat_w,
            thermal_mass_j_per_k=_THERMAL_MASS_J_PER_K_PER_KWH * battery.rated_capacity_kWh,
        )

    def open_circuit_voltage(self, soc: float) -> float:
        fraction = np.interp(soc, _OCV_SOC_BREAKPOINTS, _OCV_VOLTAGE_FRACTION)
        return float(fraction * self.nominal_voltage_v)


class BatterySensorSimulator:
    """Samples Voltage/Current/Temperature_measured, Voltage/Current_charge,
    cycle Time and discharge Capacity once per telemetry step.

    terminal_power_kw follows the controller's sign convention (positive =
    charging). charge_power_kw and discharge_power_kw are the separate,
    always-nonnegative charger-side and load-side power flows the controller
    already tracks before netting them at the terminal.
    """

    def __init__(self, model: BatteryElectricalModel, seed: int | None = None) -> None:
        self._model = model
        self._rng = np.random.default_rng(seed)
        self._voltage_noise_std_v = 0.002 * model.nominal_voltage_v
        self._current_noise_std_a = 0.5
        self._temperature_noise_std_c = 0.3
        self._temperature_c = AMBIENT_TEMP_C
        self._discharge_capacity_ahr = 0.0
        self._cycle_type = "idle"
        self._cycle_time_s = 0.0

    def step(
        self,
        *,
        soc: float,
        terminal_power_kw: float,
        charge_power_kw: float,
        discharge_power_kw: float,
        dt_s: float,
        anomaly_enabled: bool = False,
    ) -> dict:
        model = self._model
        rng = self._rng

        resistance_ohm = model.internal_resistance_ohm * (
            _ANOMALY_RESISTANCE_MULTIPLIER if anomaly_enabled else 1.0
        )
        noise_multiplier = _ANOMALY_NOISE_MULTIPLIER if anomaly_enabled else 1.0

        ocv_v = model.open_circuit_voltage(soc)
        net_current_a = terminal_power_kw * 1000 / ocv_v
        voltage_v = ocv_v + net_current_a * resistance_ohm
        voltage_measured_v = float(
            rng.normal(voltage_v, self._voltage_noise_std_v * noise_multiplier)
        )

        current_a = terminal_power_kw * 1000 / voltage_measured_v
        current_measured_a = float(
            rng.normal(current_a, self._current_noise_std_a * noise_multiplier)
        )

        heat_w = current_measured_a**2 * resistance_ohm
        cooling_w = (self._temperature_c - AMBIENT_TEMP_C) / model.thermal_resistance_k_per_w
        self._temperature_c += (heat_w - cooling_w) / model.thermal_mass_j_per_k * dt_s
        temperature_measured_c = float(
            rng.normal(self._temperature_c, self._temperature_noise_std_c * noise_multiplier)
        )

        cycle_type = (
            "charge"
            if charge_power_kw > _CYCLE_ACTIVE_POWER_KW
            else "discharge" if discharge_power_kw > _CYCLE_ACTIVE_POWER_KW else "idle"
        )
        if cycle_type != self._cycle_type:
            self._cycle_time_s = 0.0
            if cycle_type == "discharge":
                self._discharge_capacity_ahr = 0.0
        else:
            self._cycle_time_s += dt_s
        self._cycle_type = cycle_type

        cable_resistance_ohm = resistance_ohm * _CABLE_RESISTANCE_FRACTION
        if cycle_type == "charge":
            charge_current_a = charge_power_kw * 1000 / voltage_measured_v
            charge_voltage_v = voltage_measured_v + charge_current_a * cable_resistance_ohm
        elif cycle_type == "discharge":
            charge_current_a = discharge_power_kw * 1000 / voltage_measured_v
            charge_voltage_v = voltage_measured_v - charge_current_a * cable_resistance_ohm
            self._discharge_capacity_ahr += charge_current_a * dt_s / 3600
        else:
            charge_current_a = 0.0
            charge_voltage_v = voltage_measured_v

        current_charge_a = float(
            rng.normal(charge_current_a, self._current_noise_std_a * noise_multiplier)
        )
        voltage_charge_v = float(
            rng.normal(charge_voltage_v, self._voltage_noise_std_v * noise_multiplier)
        )

        return {
            "voltage_measured_v": voltage_measured_v,
            "current_measured_a": current_measured_a,
            "temperature_measured_c": temperature_measured_c,
            "current_charge_a": current_charge_a,
            "voltage_charge_v": voltage_charge_v,
            "cycle_type": cycle_type,
            "cycle_time_s": self._cycle_time_s,
            "capacity_ahr": self._discharge_capacity_ahr,
        }
