"""Declared meanings of the legacy simulators' telemetry fields."""

from dataclasses import dataclass
from fnmatch import fnmatchcase


@dataclass(frozen=True)
class Metric:
    unit: str
    value_range: tuple[float, float] | None = None
    boolean: bool = False


SHARE = Metric("fraction", (0, 1))
POWER = Metric("kW")
FIELDS: dict[str, dict[str, Metric]] = {
    "genset": {
        "load_ratio": SHARE,
        "power_kw": POWER,
        "speed_rpm": Metric("rpm"),
        "fuel_flow_kg_per_s": Metric("kg/s"),
        "bsfc_g_per_kwh": Metric("g/kWh"),
        "co2_kg_per_s": Metric("kg/s"),
        "nox_kg_per_s": Metric("kg/s"),
        "lng_level": SHARE,
        "lng_consumption_rate_kg_per_hr": Metric("kg/h"),
        "lng_time_to_empty_hr": Metric("h"),
        "air_pressure_kpa": Metric("kPa"),
        "ambient_temp_c": Metric("degC"),
        "oil_temp_c": Metric("degC"),
        "oil_pressure_bar": Metric("bar"),
        **{f"vibration_{axis}_mm_s": Metric("mm/s") for axis in ("x", "y", "z")},
        **{
            f"{field}_{index}": Metric(unit)
            for field, unit in (
                ("cylinder_pressure_bar", "bar"),
                ("cylinder_exhaust_temp_c", "degC"),
            )
            for index in range(1, 9)
        },
    },
    "battery": {
        # FEEMS reports absolute loading; the signed bus power carries direction.
        "load_ratio": SHARE,
        "power_kw": POWER,
        "charge_power_kw": POWER,
        "soc": SHARE,
        "soc_rate_per_hour": Metric("fraction/h"),
        "time_to_empty_hr": Metric("h"),
        "time_to_full_hr": Metric("h"),
        "voltage_measured_v": Metric("V"),
        "current_measured_a": Metric("A"),
        "temperature_measured_c": Metric("degC"),
        "current_charge_a": Metric("A"),
        "voltage_charge_v": Metric("V"),
        "cycle_time_s": Metric("s"),
        "capacity_ahr": Metric("Ah"),
    },
    "propulsion": {
        "load_ratio": SHARE,
        "power_output_kw": POWER,
        "power_input_kw": POWER,
        "allocated_power_kw": POWER,
        "speed_rpm": Metric("rpm"),
        "lever_position_pct": Metric("percent", (0, 100)),
        "ship_speed_knots": Metric("kn"),
        "propeller_torque_kn_m": Metric("kN.m"),
    },
    "auxload": {
        "load_ratio": SHARE,
        "power_output_kw": POWER,
        "power_input_kw": POWER,
        "allocated_power_kw": POWER,
    },
    "shore_power": {
        "connected": Metric("boolean", (0, 1), boolean=True),
        "power_ratio": SHARE,
        "input_power_kw": POWER,
        "power_kw": POWER,
        "losses_kw": POWER,
    },
    "switchboard": {
        "available_supply_kw": POWER,
        "total_demand_kw": POWER,
        "total_co2_kg_per_s": Metric("kg/s"),
        "total_nox_kg_per_s": Metric("kg/s"),
    },
    "allocation": {
        "requested_power_kw": POWER,
        "allocated_power_kw": POWER,
    },
}

DEFAULT_INCLUDE = (
    "genset.load_ratio", "genset.power_kw", "genset.speed_rpm",
    "genset.fuel_flow_kg_per_s", "genset.lng_level",
    "battery.soc", "battery.power_kw", "battery.charge_power_kw",
    "propulsion.load_ratio", "propulsion.power_input_kw", "propulsion.allocated_power_kw",
    "auxload.load_ratio", "auxload.power_input_kw", "auxload.allocated_power_kw",
    "shore_power.connected", "shore_power.power_kw",
    "switchboard.*", "allocation.*",
)


def selected_metrics(patterns: tuple[str, ...]) -> list[str]:
    return sorted(
        f"{kind}.{field}"
        for kind, fields in FIELDS.items()
        for field in fields
        if any(fnmatchcase(f"{kind}.{field}", pattern) for pattern in patterns)
    )
