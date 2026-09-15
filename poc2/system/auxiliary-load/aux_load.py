from feems.components_model.component_electric import ElectricComponent
from feems.types_for_feems import TypeComponent, TypePower


def build_auxiliary_load() -> ElectricComponent:
    return ElectricComponent(
        type_=TypeComponent.OTHER_LOAD,
        name="Aux load",
        rated_power=7040,
        power_type=TypePower.POWER_CONSUMER,
        switchboard_id=1,
    )
