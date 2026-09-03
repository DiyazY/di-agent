import numpy as np

from feems.components_model.component_electric import (
    ElectricMachine,
    ElectricComponent,
    SerialSystemElectric,
)
from feems.types_for_feems import TypeComponent, TypePower


def build_inverter() -> ElectricComponent:
    return ElectricComponent(
        type_="INVERTER",
        name="Danfoss IC7 Inverter",
        rated_power=11600,
        eff_curve=np.asarray([[1.0, 0.75, 0.5, 0.25], [0.98, 0.98, 0.97, 0.95]]).transpose(),
        power_type=TypePower.POWER_TRANSMISSION,
        switchboard_id=1,
    )


def build_rectifier() -> ElectricComponent:
    return ElectricComponent(
        type_="RECTIFIER",
        name="Danfoss IC7 Rectifier",
        rated_power=16900,
        eff_curve=np.asarray([[1.0, 0.75, 0.5, 0.25], [0.98, 0.98, 0.97, 0.95]]).transpose(),
        power_type=TypePower.POWER_TRANSMISSION,
        switchboard_id=1,
    )


def build_propeller_motor() -> ElectricMachine:
    return ElectricMachine(
        type_="ELECTRIC_MOTOR",
        name="ABB Azipod DO1400P",
        rated_power=5800,
        rated_speed=157,
        eff_curve=np.asarray([[1.0, 0.75, 0.5, 0.25], [0.95, 0.96, 0.91, 0.86]]).transpose(),
        power_type=TypePower.POWER_CONSUMER,
        switchboard_id=1,
    )


def build_propulsion_drive() -> SerialSystemElectric:
    propeller_motor = build_propeller_motor()
    return SerialSystemElectric(
        type_=TypeComponent.PROPULSION_DRIVE,
        name="Propulsion drive 1",
        power_type=TypePower.POWER_CONSUMER,
        components=[build_rectifier(), build_inverter(), propeller_motor],
        rated_power=propeller_motor.rated_power,
        rated_speed=propeller_motor.rated_speed,
        switchboard_id=1,
    )
