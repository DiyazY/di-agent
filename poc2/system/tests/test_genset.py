from pathlib import Path
from unittest.mock import MagicMock, patch

import numpy as np
import pytest
from conftest import load_component_modules
from starlette.testclient import TestClient


@pytest.fixture
def genset_modules():
    return load_component_modules("genset", ["genset", "sensors", "modbus_server", "controller", "main"])


def test_build_genset(genset_modules):
    genset_mod = genset_modules["genset"]
    genset = genset_mod.build_genset()

    assert genset.name == "Genset A"
    assert genset.aux_engine.name == "Wartsila 8V31DF"
    assert genset.aux_engine.rated_power == 4800
    assert genset.generator.name == "WEG Generator A"
    assert genset.generator.rated_power == 4225


def test_genset_sensors(genset_modules):
    sensors_mod = genset_modules["sensors"]
    rng = np.random.default_rng(42)

    # Scalar sensor test
    scalar = sensors_mod.ScalarSensor("test_sensor", 10.0, 20.0, 0.5)
    sample_zero = scalar.sample(rng, 0.0)
    sample_full = scalar.sample(rng, 1.0)
    assert 8.0 < sample_zero < 12.0
    assert 18.0 < sample_full < 22.0

    # Cylinder sensor test
    cyl_sensors = sensors_mod._build_cylinder_sensors(rng)
    assert len(cyl_sensors) == 2
    for s in cyl_sensors:
        samples = s.sample(rng, 0.5)
        assert len(samples) == sensors_mod.NUM_CYLINDERS


def test_genset_controller(genset_modules):
    Controller = genset_modules["controller"].GensetController
    controller = Controller()

    controller.set_target_load_ratio(0.75)
    assert controller.get_status()["target_load_ratio"] == 0.75

    with pytest.raises(ValueError):
        controller.set_target_load_ratio(-0.2)

    with pytest.raises(ValueError):
        controller.set_target_load_ratio(1.2)

    controller.set_anomaly_enabled(True)
    assert controller.get_status()["anomaly_enabled"] is True


def test_genset_api_endpoints(genset_modules):
    app = genset_modules["main"].app
    client = TestClient(app, raise_server_exceptions=False)

    res = client.get("/health")
    assert res.status_code in (200, 503)

    res = client.get("/status")
    assert res.status_code == 200
    assert "target_load_ratio" in res.json()

    # Load endpoints
    res = client.get("/load")
    assert res.status_code == 200
    res = client.post("/load", json={"load_ratio": 0.85})
    assert res.status_code == 200
    assert res.json()["target_load_ratio"] == 0.85

    # Anomaly endpoints
    res = client.get("/anomaly")
    assert res.status_code == 200
    res = client.post("/anomaly", json={"enabled": True})
    assert res.status_code == 200
    assert res.json()["anomaly_enabled"] is True
