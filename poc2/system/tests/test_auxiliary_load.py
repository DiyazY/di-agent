import time
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from conftest import load_component_modules
from starlette.testclient import TestClient


@pytest.fixture
def auxload_modules():
    return load_component_modules("auxiliary-load", ["aux_load", "controller", "main"])


def test_build_auxiliary_load(auxload_modules):
    aux_load = auxload_modules["aux_load"].build_auxiliary_load()
    assert aux_load.name == "Aux load"
    assert aux_load.rated_power == 7040
    assert aux_load.switchboard_id == 1


def test_make_event(auxload_modules):
    _make_event = auxload_modules["controller"]._make_event
    payload = {"load_ratio": 0.5, "power_kw": 3500.0}
    event = _make_event("auxload-1", "auxload", payload)

    assert event["event"] == "auxload.telemetry"
    assert event["schema_version"] == 1
    assert event["source_id"] == "auxload-1"
    assert event["source_type"] == "auxload"
    assert event["load_ratio"] == 0.5
    assert "timestamp" in event


def test_aux_load_controller_set_target(auxload_modules):
    Controller = auxload_modules["controller"].AuxLoadController
    controller = Controller()

    controller.set_target_load_ratio(0.75)
    status = controller.get_status()
    assert status["target_load_ratio"] == 0.75

    with pytest.raises(ValueError):
        controller.set_target_load_ratio(-0.1)

    with pytest.raises(ValueError):
        controller.set_target_load_ratio(1.5)


def test_aux_load_controller_allocation_stale(auxload_modules):
    Controller = auxload_modules["controller"].AuxLoadController
    controller = Controller()

    # Initial allocation is None -> 0.0
    with controller._lock:
        assert controller._get_allocated_power_kw() == 0.0

        # Fresh allocation
        controller._allocation = (5000.0, time.time())
        assert controller._get_allocated_power_kw() == 5000.0

        # Stale allocation (> 5s ago)
        controller._allocation = (5000.0, time.time() - 10.0)
        assert controller._get_allocated_power_kw() == 0.0


def test_aux_load_api_endpoints(auxload_modules):
    app = auxload_modules["main"].app
    client = TestClient(app, raise_server_exceptions=False)

    # Health check
    res = client.get("/health")
    assert res.status_code in (200, 503)

    # Get Status
    res = client.get("/status")
    assert res.status_code == 200
    assert "target_load_ratio" in res.json()

    # Get /load
    res = client.get("/load")
    assert res.status_code == 200
    assert "target_load_ratio" in res.json()

    # POST /load valid
    res = client.post("/load", json={"load_ratio": 0.6})
    assert res.status_code == 200
    assert res.json()["target_load_ratio"] == 0.6

    # POST /load invalid
    res = client.post("/load", json={"load_ratio": 2.0})
    assert res.status_code == 422
