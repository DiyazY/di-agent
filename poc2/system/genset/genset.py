import numpy as np

from feems.components_model.component_mechanical import EngineDualFuel
from feems.components_model.component_electric import (
    ElectricMachine,
    Genset
)
from feems.fuel import TypeFuel, FuelOrigin
from feems.types_for_feems import EngineCycleType, NOxCalculationMethod, TypePower


def build_auxiliary_engine() -> EngineDualFuel:
    return EngineDualFuel(
        type_="MAIN_ENGINE",
        name="Wartsila 8V31DF",
        rated_power=4800,
        rated_speed=750,
        bsfc_curve=np.asarray([[0.5, 0.75, 0.85, 1.0], [153.5, 145.9, 144.2, 142.8]]).transpose(),
        fuel_type=TypeFuel.NATURAL_GAS,
        fuel_origin=FuelOrigin.BIO,
        bspfc_curve=np.asarray([[0.5, 0.75, 0.85, 1.0], [8.8, 6.0, 5.6, 4.7]]).transpose(),
        pilot_fuel_type=TypeFuel.LFO,
        pilot_fuel_origin=FuelOrigin.BIO,
        engine_cycle_type=EngineCycleType.OTTO,
        nox_calculation_method=NOxCalculationMethod.TIER_3,
    )


def build_generator() -> ElectricMachine:
    return ElectricMachine(
        type_="GENERATOR",
        name="WEG Generator A",
        rated_power=4225,
        rated_speed=750,
        power_type=TypePower.POWER_SOURCE,
        switchboard_id=1,
        eff_curve=np.asarray([[0.25, 0.5, 0.75, 0.85, 1.0], [0.915, 0.951, 0.962, 0.964, 0.9602]]).transpose(),
    )


def build_genset() -> Genset:
    return Genset(name="Genset A", aux_engine=build_auxiliary_engine(), generator=build_generator())
