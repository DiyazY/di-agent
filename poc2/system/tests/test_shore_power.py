from pathlib import Path

import pytest
from conftest import load_component_modules
from starlette.testclient import TestClient


@pytest.fixture
def shore_power_modules():
    return load_component_modules("shore-power", ["shore_power", "controller", "main"])


def test_build_shore_power_system(shore_power_modules):
    shore_mod = shore_power_modules["shore_power"]
    sys_obj = shore_mod.build_shore_power_system(rated_power_kw=1500.0, switchboard_id=1)

    assert sys_obj.name == "Shore Power System"
    assert sys_obj.shore_power_connection.rated_power == 1500.0
    assert sys_obj.converter.rated_power == 1500.0
    assert sys_obj.switchboard_id == 1


def test_shore_power_controller(shore_power_modules):
    Controller = shore_power_modules["controller"].ShorePowerController
    controller = Controller()

    controller.set_connected(True)
    assert controller.get_status()["connected"] is True

    controller.set_target_power_ratio(0.85)
    assert controller.get_status()["target_power_ratio"] == 0.85

    with pytest.raises(ValueError):
        controller.set_target_power_ratio(-0.1)

    with pytest.raises(ValueError):
        controller.set_target_power_ratio(1.5)


def test_shore_power_api_endpoints(shore_power_modules):
    app = shore_power_modules["main"].app
    client = TestClient(app, raise_server_exceptions=False)

    res = client.get("/health")
    assert res.status_code in (200, 503)

    res = client.get("/status")
    assert res.status_code == 200
    assert "connected" in res.json()

    res = client.post("/connect", json={"connected": True})
    assert res.status_code == 200
    assert res.json()["connected"] is True

    res = client.get("/power")
    assert res.status_code == 200

    res = client.post("/power", json={"power_ratio": 0.7})
    assert res.status_code == 200
    assert res.json()["target_power_ratio"] == 0.7
