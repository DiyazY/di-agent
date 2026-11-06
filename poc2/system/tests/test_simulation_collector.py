import json
from unittest.mock import MagicMock

import pytest
from fastapi import HTTPException
from starlette.testclient import TestClient

from conftest import load_component_modules


NOW = 1793895000


@pytest.fixture
def module(monkeypatch):
    module = load_component_modules("simulation-collector", ["schema", "main"])["main"]
    monkeypatch.setattr(module.time, "time", lambda: NOW)
    return module


@pytest.fixture
def collector(module):
    collector = module.SimulationCollector()
    collector.hello({
        "protocol": 1, "name": "simulation", "node_id": "worker-1",
        "config": {"include": ["*"]},
    })
    return collector


def ingest(collector, kind, fields, *, device="one", timestamp=NOW):
    return collector.ingest_json(
        f"{kind}.telemetry",
        json.dumps({f"{kind}_id": device, "timestamp": timestamp, **fields}),
        now=NOW,
    )


def collect(collector, seq=1):
    return collector.collect({"seq": seq, "now_unix": NOW + seq})


@pytest.mark.parametrize("kind", ["genset", "battery", "propulsion", "auxload", "shore_power"])
def test_all_declared_fields_have_units_ranges_and_device_identity(module, collector, kind):
    fields = {
        field: True if declaration.boolean else 0.5
        for field, declaration in module.FIELDS[kind].items()
    }
    for device in ("one", "two"):
        assert ingest(collector, kind, fields, device=device)
    reply = collect(collector)
    assert reply["seq"] == 1
    assert len(reply["samples"]) == len(fields) * 2
    for sample in reply["samples"]:
        field = sample["metric_type"].split(".", 1)[1]
        declaration = module.FIELDS[kind][field]
        assert sample["unit"] == declaration.unit
        assert sample["subject"] in (f"{kind}:one", f"{kind}:two")
        assert sample["timestamp_unix"] == NOW
        assert sample["chart"] == kind
        assert sample["metric_type"] in reply["available"]
        assert sample.get("range") == (
            list(declaration.value_range) if declaration.value_range is not None else None
        )
        assert not {"node_id", "event_id", "source"} & sample.keys()
    assert collect(collector, 2)["samples"] == []


def test_latest_snapshot_not_mean_and_same_second_not_reemitted(collector):
    assert ingest(collector, "genset", {"power_kw": 100}, timestamp=NOW - 1)
    assert ingest(collector, "genset", {"power_kw": 300}, timestamp=NOW + 0.1)
    assert collect(collector)["samples"][0]["value"] == 300
    assert ingest(collector, "genset", {"power_kw": 500}, timestamp=NOW + 0.2)
    assert collect(collector, 2)["samples"] == []
    assert ingest(collector, "genset", {"power_kw": 600}, timestamp=NOW + 1)
    assert collect(collector, 3)["samples"][0]["value"] == 600


def test_optional_predictions_do_not_survive_newer_snapshot(collector):
    assert ingest(collector, "battery", {"soc": 0.7, "time_to_empty_hr": 2}, timestamp=NOW - 1)
    assert ingest(collector, "battery", {"soc": 0.8, "time_to_full_hr": 1})
    samples = collect(collector)["samples"]
    assert {s["metric_type"] for s in samples} == {"battery.soc", "battery.time_to_full_hr"}


def test_switchboard_totals_once_and_allocations_per_consumer(collector):
    for consumer, power in (("propulsion-1", 100), ("auxload-1", 20)):
        assert ingest(collector, "switchboard", {
            "consumer_id": consumer, "allocated_power_kw": power,
            "requested_power_kw": power + 10, "available_supply_kw": 150,
            "total_demand_kw": 140, "total_co2_kg_per_s": 0.1,
        })
    samples = collect(collector)["samples"]
    assert len(samples) == 7
    assert len([s for s in samples if s["metric_type"] == "switchboard.available_supply_kw"]) == 1
    assert {
        s["subject"]: s["value"] for s in samples if s["metric_type"] == "allocation.allocated_power_kw"
    } == {"allocation:one:propulsion-1": 100, "allocation:one:auxload-1": 20}


def test_signed_power_boolean_and_labels(collector):
    assert ingest(collector, "battery", {"power_kw": -500, "cycle_type": "charge"})
    assert ingest(collector, "shore_power", {"connected": False})
    samples = collect(collector)["samples"]
    battery = next(s for s in samples if s["metric_type"] == "battery.power_kw")
    assert battery["value"] == -500
    assert "range" not in battery
    assert battery["labels"]["cycle_type"] == "charge"
    shore = next(s for s in samples if s["metric_type"] == "shore_power.connected")
    assert shore["value"] == 0 and shore["range"] == [0, 1]


def test_envelope_is_authoritative_not_duplicated_flat_fields(collector):
    message = {
        "source_id": "g1", "source_type": "genset", "schema_version": 1,
        "timestamp": NOW, "power_kw": 999,
        "payload": {"genset_id": "g1", "power_kw": 100},
    }
    assert collector.ingest_json("genset.telemetry", json.dumps(message), now=NOW)
    assert collect(collector)["samples"][0]["value"] == 100


@pytest.mark.parametrize("raw", [
    "bad json", "[]", "null", b"\xff",
    json.dumps({"payload": []}),
    json.dumps({"genset_id": "g1", "timestamp": True}),
    json.dumps({"genset_id": "g1", "timestamp": NOW * 1000}),
    json.dumps({"genset_id": "g1", "timestamp": NOW - 16}),
    json.dumps({"genset_id": "x" * 201, "timestamp": NOW}),
    json.dumps({"genset_id": "g1", "timestamp": NOW, "source_type": "battery"}),
    json.dumps({"genset_id": "g1", "timestamp": NOW, "source_id": "g2"}),
    json.dumps({"genset_id": "g1", "timestamp": NOW, "schema_version": 2}),
])
def test_invalid_events_reported_and_do_not_poison_valid_events(collector, caplog, raw):
    assert not collector.ingest_json("genset.telemetry", raw, now=NOW)
    assert collector.status()["dropped"] == 1
    assert "Dropping simulation message" in caplog.text
    assert ingest(collector, "genset", {"power_kw": 1})
    assert collect(collector)["samples"][0]["value"] == 1


@pytest.mark.parametrize("bad", [float("nan"), float("inf"), 10**400, True, "12", None, []])
def test_invalid_field_is_logged_while_other_fields_survive(collector, caplog, bad):
    assert ingest(collector, "genset", {"power_kw": bad, "load_ratio": 0.3})
    assert [s["metric_type"] for s in collect(collector)["samples"]] == ["genset.load_ratio"]
    assert collector.status()["invalid_fields"] == 1
    assert "Skipping invalid simulation fields" in caplog.text


def test_expired_pending_data_and_out_of_order_messages(module, collector, monkeypatch):
    assert ingest(collector, "genset", {"power_kw": 2}, timestamp=NOW - 2)
    assert ingest(collector, "genset", {"power_kw": 1}, timestamp=NOW - 3)
    assert collect(collector)["samples"][0]["value"] == 2
    assert ingest(collector, "battery", {"soc": 0.7})
    monkeypatch.setattr(module.time, "time", lambda: NOW + 16)
    assert collect(collector, 2)["samples"] == []
    assert collector.status()["expired"] == 1
    assert collector._snapshots == {}


def test_capacity_bound_and_expiration_releases_capacity(module, caplog, monkeypatch):
    collector = module.SimulationCollector(max_active_series=1)
    collector.hello({"protocol": 1, "name": "simulation", "node_id": "worker-1"})
    assert ingest(collector, "genset", {"power_kw": 1})
    assert not ingest(collector, "genset", {"power_kw": 2}, device="two")
    assert "active series limit" in caplog.text
    monkeypatch.setattr(module.time, "time", lambda: NOW + 16)
    assert collector.ingest_json(
        "genset.telemetry", json.dumps({"genset_id": "two", "power_kw": 2, "timestamp": NOW + 16}),
    )
    assert collect(collector)["samples"][0]["subject"] == "genset:two"


def test_curated_default_and_explicit_selection(module):
    collector = module.SimulationCollector()
    hello = {"protocol": 1, "name": "simulation", "node_id": "worker-1"}
    assert collector.hello(hello) == {"protocol": 1}
    assert ingest(collector, "genset", {"power_kw": 2, "oil_temp_c": 80})
    assert [s["metric_type"] for s in collect(collector)["samples"]] == ["genset.power_kw"]
    collector.hello({**hello, "config": {"include": ["genset.*temp_c*"]}})
    assert ingest(collector, "genset", {"power_kw": 2, "oil_temp_c": 80})
    assert [s["metric_type"] for s in collect(collector)["samples"]] == ["genset.oil_temp_c"]


def test_encoded_subjects_are_stable_and_collision_free(module, collector):
    assert ingest(collector, "genset", {"power_kw": 1}, device="Genset A")
    assert ingest(collector, "genset", {"power_kw": 2}, device="hex-47656e7365742041")
    samples = collect(collector)["samples"]
    assert len({s["subject"] for s in samples}) == 2
    assert {s["labels"]["source_id"] for s in samples} == {"Genset A", "hex-47656e7365742041"}
    assert all(module.SUBJECT_RE.fullmatch(s["subject"]) for s in samples)
    assert module.subject_segment("a:b") != module.subject_segment("a") + ":b"


def test_protocol_validation_and_single_owner(module, collector):
    with pytest.raises(HTTPException, match="hello"):
        module.SimulationCollector().collect({"seq": 0, "now_unix": NOW})
    for request in (
        {"seq": True, "now_unix": NOW}, {"seq": -1, "now_unix": NOW},
        {"seq": 1, "now_unix": 0}, {"seq": 1, "now_unix": False},
    ):
        with pytest.raises(HTTPException):
            collector.collect(request)
    with pytest.raises(HTTPException, match="another agent"):
        collector.hello({"protocol": 1, "name": "simulation", "node_id": "worker-2"})
    for config in ({"include": []}, {"include": ["unknown.*"]}, {"unknown": 1}):
        with pytest.raises(HTTPException):
            collector.hello({"protocol": 1, "name": "simulation", "node_id": "worker-1", "config": config})


def test_http_health_ready_and_protocol(module, collector, monkeypatch):
    monkeypatch.setattr(module, "collector", collector)
    monkeypatch.setattr(collector, "start", lambda: None)
    monkeypatch.setattr(collector, "stop", lambda: None)
    with TestClient(module.app) as client:
        assert client.get("/health").status_code == 503
        assert client.get("/ready").status_code == 503
        collector._connected = True
        collector._thread = MagicMock()
        assert client.get("/health").status_code == 200
        assert client.get("/ready").status_code == 200
        assert client.post("/collect", json={"op": "wrong"}).status_code == 400
        response = client.post("/collect", json={"op": "collect", "seq": 5, "now_unix": NOW})
        assert response.json()["seq"] == 5


def test_kafka_subscription_independent_group_and_raw_deserialization(module, monkeypatch):
    consumer = MagicMock()
    factory = MagicMock(return_value=consumer)
    monkeypatch.setattr(module, "KafkaConsumer", factory)
    assert module.SimulationCollector()._new_consumer() is consumer
    assert set(factory.call_args.args) == set(module.TOPICS)
    assert factory.call_args.kwargs["group_id"] == "simulation-collector"
    assert factory.call_args.kwargs["auto_offset_reset"] == "latest"
    assert "value_deserializer" not in factory.call_args.kwargs


def test_kafka_reconnect_and_unexpected_worker_failure_are_visible(module, monkeypatch, caplog):
    collector = module.SimulationCollector()
    consumer = MagicMock()
    consumer.poll.side_effect = RuntimeError("unexpected worker failure")
    factory = MagicMock(side_effect=[module.KafkaError("broker unavailable"), consumer])
    monkeypatch.setattr(collector, "_new_consumer", factory)
    monkeypatch.setattr(collector._stop, "wait", lambda _: False)
    collector._consume()
    assert factory.call_count == 2
    consumer.close.assert_called_once()
    assert collector.status()["connected"] is False
    assert collector.status()["last_error"] == "unexpected worker failure"
    assert "reconnecting" in caplog.text and "stopped unexpectedly" in caplog.text


@pytest.mark.parametrize(("component", "names", "class_name", "kind"), [
    ("genset", ["genset", "sensors", "modbus_server", "controller"], "GensetController", "genset"),
    ("battery", ["battery", "sensors", "modbus_server", "controller"], "BatteryController", "battery"),
    ("propulsion", ["propulsion", "sensors", "modbus_server", "controller"], "PropulsionController", "propulsion"),
    ("auxiliary-load", ["aux_load", "controller"], "AuxLoadController", "auxload"),
    ("shore-power", ["shore_power", "controller"], "ShorePowerController", "shore_power"),
    ("switchboard", ["switchboard", "modbus_client", "controller"], "SwitchboardController", "switchboard"),
])
def test_actual_producer_event_reaches_collector_and_writer(
    collector, component, names, class_name, kind, monkeypatch,
):
    loaded = load_component_modules(component, names)
    controller = getattr(loaded["controller"], class_name)()
    controller._producer = MagicMock()
    controller._stop_event = MagicMock()
    controller._stop_event.is_set.side_effect = [False, True]
    if kind == "shore_power":
        monkeypatch.setattr(controller, "_charge_battery", lambda _: None)
    if kind == "switchboard":
        controller._consumer_requests["propulsion-1"] = (100, 1, NOW)
    controller._run_loop()
    events = [
        call.kwargs["value"] for call in controller._producer.send.call_args_list
        if call.args[0] == f"{kind}.telemetry"
    ]
    assert events
    writer = load_component_modules("telemetry-writer", ["main"])["main"]
    for event in events:
        assert writer._to_points(event, f"{kind}.telemetry")
        assert collector.ingest_json(f"{kind}.telemetry", json.dumps(event), now=NOW)
    reply = collect(collector)
    assert reply["samples"]
    payload = events[-1]["payload"]
    emitted_fields = {s["metric_type"].split(".", 1)[1] for s in reply["samples"]}
    numeric_fields = {
        key for key, value in payload.items()
        if isinstance(value, (int, float)) and key != "timestamp"
    }
    assert numeric_fields <= emitted_fields
