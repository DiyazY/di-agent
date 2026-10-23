import time
from pathlib import Path

import pytest
from conftest import load_component_modules
from starlette.testclient import TestClient


@pytest.fixture
def propulsion_modules():
    return load_component_modules("propulsion", ["propulsion", "sensors", "modbus_server", "controller", "main"])


def test_build_propulsion_drive(propulsion_modules):
    prop_mod = propulsion_modules["propulsion"]
    drive = prop_mod.build_propulsion_drive()

    assert drive.name == "Propulsion drive 1"
    assert drive.rated_power == 5800
    assert drive.rated_speed == 157
    assert len(drive.components) == 3


def test_propulsion_sensors(propulsion_modules):
    sensors_mod = propulsion_modules["sensors"]
    sim = sensors_mod.SensorSimulator(seed=42)

    res_normal = sim.simulate(
        target_load_ratio=0.8,
        load_ratio=0.8,
        power_output_kw=4640.0,
        speed_rpm=157.0,
        anomaly_enabled=False,
    )
    assert "lever_position_pct" in res_normal
    assert "ship_speed_knots" in res_normal
    assert "propeller_torque_kn_m" in res_normal

    res_anomaly = sim.simulate(
        target_load_ratio=0.8,
        load_ratio=0.8,
        power_output_kw=4640.0,
        speed_rpm=157.0,
        anomaly_enabled=True,
    )
    # Anomaly produces lower ship speed and higher torque
    assert res_anomaly["ship_speed_knots"] < res_normal["ship_speed_knots"]
    assert res_anomaly["propeller_torque_kn_m"] > res_normal["propeller_torque_kn_m"]


def test_propulsion_controller(propulsion_modules):
    Controller = propulsion_modules["controller"].PropulsionController
    controller = Controller()

    controller.set_target_load_ratio(0.65)
    status = controller.get_status()
    assert status["target_load_ratio"] == 0.65

    with pytest.raises(ValueError):
        controller.set_target_load_ratio(-0.1)

    with pytest.raises(ValueError):
        controller.set_target_load_ratio(1.1)

    controller.set_anomaly_enabled(True)
    assert controller.get_status()["anomaly_enabled"] is True

    # Test stale allocation check
    with controller._lock:
        assert controller._get_allocated_power_kw() == 0.0
        controller._allocation = (4000.0, time.time())
        assert controller._get_allocated_power_kw() == 4000.0
        controller._allocation = (4000.0, time.time() - 10.0)
        assert controller._get_allocated_power_kw() == 0.0


def test_propulsion_api_endpoints(propulsion_modules):
    app = propulsion_modules["main"].app
    client = TestClient(app, raise_server_exceptions=False)

    res = client.get("/health")
    assert res.status_code in (200, 503)

    res = client.get("/status")
    assert res.status_code == 200
    assert "target_load_ratio" in res.json()

    # Load endpoints
    res = client.get("/load")
    assert res.status_code == 200
    res = client.post("/load", json={"load_ratio": 0.55})
    assert res.status_code == 200
    assert res.json()["target_load_ratio"] == 0.55

    # Anomaly endpoints
    res = client.get("/anomaly")
    assert res.status_code == 200
    res = client.post("/anomaly", json={"enabled": True})
    assert res.status_code == 200
    assert res.json()["anomaly_enabled"] is True
