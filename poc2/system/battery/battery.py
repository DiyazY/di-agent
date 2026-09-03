from feems.components_model.component_electric import Battery


def build_battery() -> Battery:
    return Battery(
        name="AYK LFP Battery",
        rated_capacity_kwh=10400,
        charging_rate_c=1,
        discharge_rate_c=1,
        switchboard_id=1,
    )
