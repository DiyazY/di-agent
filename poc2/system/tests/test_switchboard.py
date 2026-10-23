import time
from pathlib import Path

import pytest
from conftest import load_component_modules
from starlette.testclient import TestClient


@pytest.fixture
def switchboard_modules():
    return load_component_modules("switchboard", ["switchboard", "modbus_client", "controller", "main"])


def test_allocate_power_priority(switchboard_modules):
    swb_mod = switchboard_modules["switchboard"]
    ConsumerRequest = swb_mod.ConsumerRequest
    allocate_power = swb_mod.allocate_power

    requests = [
        ConsumerRequest(consumer_id="auxload", requested_power_kw=1000.0, priority=1, received_at=time.time()),
        ConsumerRequest(consumer_id="propulsion", requested_power_kw=3000.0, priority=2, received_at=time.time()),
    ]

    # Ample supply: both get full demand
    alloc_full = allocate_power(5000.0, requests)
    assert alloc_full["propulsion"] == 3000.0
    assert alloc_full["auxload"] == 1000.0

    # Deficit: priority 2 (propulsion) gets served first (3000 kW), leaving 500 kW for auxload
    alloc_partial = allocate_power(3500.0, requests)
    assert alloc_partial["propulsion"] == 3000.0
    assert alloc_partial["auxload"] == 500.0

    # Severe deficit: propulsion gets partial, auxload gets 0
    alloc_starved = allocate_power(2000.0, requests)
    assert alloc_starved["propulsion"] == 2000.0
    assert alloc_starved["auxload"] == 0.0

    # Zero or negative supply
    alloc_zero = allocate_power(0.0, requests)
    assert alloc_zero["propulsion"] == 0.0
    assert alloc_zero["auxload"] == 0.0


def test_summarize_source_health(switchboard_modules):
    summarize = switchboard_modules["switchboard"].summarize_source_health

    gensets = {
        "g1": {"stale": False},
        "g2": {"stale": True},
    }
    batteries = {
        "b1": {"stale": False},
    }
    consumers = {}

    summary = summarize(gensets, batteries, consumers)
    assert summary["gensets"] == {"total": 2, "stale": 1, "healthy": 1}
    assert summary["batteries"] == {"total": 1, "stale": 0, "healthy": 1}
    assert summary["consumers"] == {"total": 0, "stale": 0, "healthy": 0}


def test_modbus_target_parsing(switchboard_modules):
    modbus_mod = switchboard_modules["modbus_client"]

    raw = "genset-1@genset-1:5020, genset-2@192.168.1.50:5021"
    targets = modbus_mod._parse_targets(raw)
    assert len(targets) == 2
    assert targets[0] == ("genset-1", "genset-1", 5020)
    assert targets[1] == ("genset-2", "192.168.1.50", 5021)

    assert modbus_mod._parse_targets("") == []


def test_deserialize_kafka_payload(switchboard_modules):
    deserialize = switchboard_modules["controller"]._deserialize_value

    # Valid JSON dict
    assert deserialize(b'{"key": "value"}') == {"key": "value"}
    # Invalid JSON
    assert deserialize(b'{malformed') is None
    # Valid JSON but non-dict
    assert deserialize(b'[1, 2, 3]') is None


def test_switchboard_api_endpoints(switchboard_modules):
    app = switchboard_modules["main"].app
    client = TestClient(app, raise_server_exceptions=False)

    res = client.get("/health")
    assert res.status_code in (200, 503)

    res = client.get("/status")
    assert res.status_code == 200
    assert "available_supply_kw" in res.json()

    res = client.get("/modbus")
    assert res.status_code == 200
