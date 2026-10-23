from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from conftest import load_component_modules
from starlette.testclient import TestClient


@pytest.fixture
def battery_modules():
    return load_component_modules("battery", ["battery", "sensors", "modbus_server", "controller", "main"])


def test_build_battery_models(battery_modules):
    battery_mod = battery_modules["battery"]

    ayk = battery_mod.build_battery("ayk-lfp")
    assert ayk.name == "AYK LFP Battery"
    assert ayk.rated_capacity_kWh == 10400

    leclanche = battery_mod.build_battery("leclanche-mrs2")
    assert leclanche.name == "Leclanche MRS-2 Battery"
    assert leclanche.rated_capacity_kWh == 2200

    with pytest.raises(ValueError):
        battery_mod.build_battery("invalid-model")


def test_battery_electrical_model_and_sensors(battery_modules):
    sensors_mod = battery_modules["sensors"]
    battery_mod = battery_modules["battery"]

    battery = battery_mod.build_battery("ayk-lfp")
    model = sensors_mod.BatteryElectricalModel.from_battery(battery, 1000.0)

    assert model.nominal_voltage_v == 1000.0
    assert model.internal_resistance_ohm > 0
    assert model.thermal_resistance_k_per_w > 0
    assert model.thermal_mass_j_per_k > 0

    # Test open circuit voltage curve
    v_50 = model.open_circuit_voltage(0.5)
    assert pytest.approx(v_50, rel=1e-2) == 1000.0
    v_100 = model.open_circuit_voltage(1.0)
    assert v_100 > v_50

    # Sensor simulator
    sim = sensors_mod.BatterySensorSimulator(model, seed=42)
    sample = sim.step(
        soc=0.8,
        terminal_power_kw=100.0,
        charge_power_kw=100.0,
        discharge_power_kw=0.0,
        dt_s=1.0,
        anomaly_enabled=False,
    )
    assert "voltage_measured_v" in sample
    assert "current_measured_a" in sample
    assert "temperature_measured_c" in sample
    assert sample["cycle_type"] == "charge"


def test_battery_modbus_float_encoding(battery_modules):
    modbus_mod = battery_modules["modbus_server"]

    original_val = 123.456
    encoded = modbus_mod._encode_float(original_val)
    assert len(encoded) == 2
    decoded = modbus_mod._decode_float(encoded)
    assert pytest.approx(decoded, rel=1e-4) == original_val


def test_battery_controller(battery_modules):
    Controller = battery_modules["controller"].BatteryController
    controller = Controller()

    controller.set_target_load_ratio(0.5)
    status = controller.get_status()
    assert status["target_load_ratio"] == 0.5

    with pytest.raises(ValueError):
        controller.set_target_load_ratio(1.5)

    controller.set_target_charge_power_kw(500.0)
    status = controller.get_status()
    assert status["target_charge_power_kw"] == 500.0

    with pytest.raises(ValueError):
        controller.set_target_charge_power_kw(-10.0)

    controller.set_anomaly_enabled(True)
    assert controller.get_status()["anomaly_enabled"] is True


def test_battery_api_endpoints(battery_modules):
    app = battery_modules["main"].app
    client = TestClient(app, raise_server_exceptions=False)

    res = client.get("/health")
    assert res.status_code in (200, 503)

    res = client.get("/status")
    assert res.status_code == 200
    assert "soc" in res.json()

    # Load endpoints
    res = client.get("/load")
    assert res.status_code == 200
    res = client.post("/load", json={"load_ratio": 0.4})
    assert res.status_code == 200
    assert res.json()["target_load_ratio"] == 0.4

    # Charge endpoints
    res = client.get("/charge")
    assert res.status_code == 200
    res = client.post("/charge", json={"power_kw": 250.0})
    assert res.status_code == 200
    assert res.json()["target_charge_power_kw"] == 250.0

    # Anomaly endpoints
    res = client.get("/anomaly")
    assert res.status_code == 200
    res = client.post("/anomaly", json={"enabled": True})
    assert res.status_code == 200
    assert res.json()["anomaly_enabled"] is True
