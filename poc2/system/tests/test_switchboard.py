import time
from types import SimpleNamespace
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from conftest import api_test_client, load_component_modules


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
    main = switchboard_modules["main"]
    with api_test_client(main.app, main.controller) as client:
        res = client.get("/health")
        assert res.status_code == 503

        res = client.get("/status")
        assert res.status_code == 200
        assert "available_supply_kw" in res.json()

        res = client.get("/modbus")
        assert res.status_code == 200


def test_modbus_server_registers_decode_as_switchboard_supply(switchboard_modules, monkeypatch):
    from conftest import load_component_modules

    genset_server = load_component_modules("genset", ["modbus_server"])["modbus_server"]
    polling_client = switchboard_modules["modbus_client"]
    server = genset_server.GensetModbusServer(controller=MagicMock())
    server.sync_from_status({
        "target_load_ratio": 0.5,
        "current_load_ratio": 0.4,
        "speed_rpm": 1200,
        "anomaly_enabled": False,
        "last_message": {"power_kw": 1800, "co2_kg_per_s": 0.12, "nox_kg_per_s": 0.03},
    })
    registers = server._input.getValues(0, 10)
    fake_client = MagicMock()
    fake_client.read_input_registers.return_value = SimpleNamespace(
        registers=registers,
        isError=lambda: False,
    )

    decoded = polling_client._GensetPoller.read(fake_client)

    assert decoded == {
        "current_load_ratio": pytest.approx(0.4),
        "power_kw": pytest.approx(1800),
        "speed_rpm": pytest.approx(1200),
        "co2_kg_per_s": pytest.approx(0.12),
        "nox_kg_per_s": pytest.approx(0.03),
    }


def test_unexpected_modbus_poller_error_is_reported(switchboard_modules, monkeypatch):
    modbus_module = switchboard_modules["modbus_client"]
    fake_client = MagicMock()
    fake_client.connected = True
    monkeypatch.setattr(modbus_module, "ModbusTcpClient", lambda *_args, **_kwargs: fake_client)
    monkeypatch.setattr(
        modbus_module._GensetPoller,
        "read",
        MagicMock(side_effect=RuntimeError("bad register response")),
    )
    poller = modbus_module.ModbusPollingClient("genset-1@localhost:5020", "", 0.01)

    poller.start()
    deadline = time.monotonic() + 1
    while poller.get_health()["last_error"] is None and time.monotonic() < deadline:
        time.sleep(0.01)
    health = poller.get_health()
    status = poller.get_status()
    poller.stop()

    assert health["status"] == "error"
    assert health["thread_running"] is True
    assert "bad register response" in health["last_error"]
    assert status["genset-1"]["stale"] is True


def test_stale_modbus_supply_is_excluded_from_allocation(switchboard_modules, monkeypatch):
    controller = switchboard_modules["controller"].SwitchboardController()
    monkeypatch.setattr(
        controller._modbus_client,
        "get_status",
        lambda: {
            "genset-1": {
                "source_type": "genset",
                "stale": True,
                "fetched_at": time.time(),
                "power_kw": 1800,
                "co2_kg_per_s": 0.12,
                "nox_kg_per_s": 0.03,
            },
        },
    )

    status = controller.get_status()

    assert status["gensets"]["genset-1"]["stale"] is True
    assert status["available_supply_kw"] == 0
