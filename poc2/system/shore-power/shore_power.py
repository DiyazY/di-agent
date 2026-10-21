import numpy as np

from feems.components_model.component_electric import (
    ElectricComponent,
    ShorePowerConnection,
    ShorePowerConnectionSystem,
)
from feems.types_for_feems import Power_kW, SwbId, TypeComponent, TypePower

# Converter efficiency curve: [load ratio, efficiency], typical for a shore power
# frequency/voltage converter (96-98% efficient).
CONVERTER_EFF_CURVE = np.array(
    [
        [0.25, 0.5, 0.75, 1.0],
        [0.96, 0.97, 0.98, 0.98],
    ]
).T


def build_shore_power_system(
    rated_power_kw: float = 1000.0, switchboard_id: int = 1
) -> ShorePowerConnectionSystem:
    shore_power_connection = ShorePowerConnection(
        name="Shore Power Connection",
        rated_power=Power_kW(rated_power_kw),
        switchboard_id=SwbId(switchboard_id),
    )
    converter = ElectricComponent(
        type_=TypeComponent.POWER_CONVERTER,
        name="Shore Power Converter",
        rated_power=Power_kW(rated_power_kw),
        power_type=TypePower.POWER_TRANSMISSION,
        switchboard_id=SwbId(switchboard_id),
        eff_curve=CONVERTER_EFF_CURVE,
    )
    return ShorePowerConnectionSystem(
        name="Shore Power System",
        shore_power_connection=shore_power_connection,
        converter=converter,
        switchboard_id=SwbId(switchboard_id),
    )
