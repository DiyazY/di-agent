import json
from unittest.mock import MagicMock, patch

import pytest

from conftest import load_component_modules


@pytest.mark.parametrize("mode", ["synthetic", "real"])
def test_postgres_metadata_without_units_reaches_collector(mode):
    replay = load_component_modules("replay-streamer", ["main"])["main"]
    collector_module = load_component_modules("dataset-collector", ["main"])["main"]
    profile = replay.SignalProfile("short", "Injection", -20, -10, -15, 2, 2, 0, 1)
    config = replay.StreamConfig(mode, "default-run", 1000, 1, False, 7)
    connection = MagicMock()
    connection.execute.return_value.fetchall.return_value = [("Bore", "200")]
    connection.cursor.return_value.__enter__.return_value = MagicMock()
    with (
        patch.object(replay.psycopg, "connect") as connect,
        patch.object(replay, "_profiles", return_value=[profile]),
        patch.object(replay, "_series_rows", return_value=iter([
            ("short", "Injection", 0, -20), ("short", "Injection", 1, -10),
        ])),
    ):
        connect.return_value.__enter__.return_value = connection
        records = list(
            replay.synthetic_records(config) if mode == "synthetic" else replay.real_records(config)
        )

    collector = collector_module.DatasetCollector()
    collector.hello({"protocol": 1, "name": "dataset", "node_id": "agent-1"})
    assert len(records) == 2
    for record in records:
        assert record["metadata"]["Bore"] == "200"
        assert collector.ingest_json(json.dumps(record))
    result = collector.collect({"seq": 1, "now_unix": 1761200000})
    sample = result["samples"][0]
    assert sample["unit"] == "dataset-native"
    assert sample["range"] == [-20, -10]
    assert sample["subject"] == "engine:default-run"
    assert -20 <= sample["value"] <= -10
    assert sample["labels"]["origin"] == mode
    assert collector.status()["received"] == 2
    assert collector.status()["dropped"] == 0


@pytest.mark.parametrize("mode", ["synthetic", "real"])
@pytest.mark.parametrize("run_id", [
    "1000rpm_150kW_0001", "1000rpm_150kW_0001_2",
    "925rpm_100kW_0001", "925rpm_100kW_0001_2",
])
def test_added_engine_signals_reach_writer_and_collector(mode, run_id):
    replay = load_component_modules("replay-streamer", ["main"])["main"]
    writer = load_component_modules("telemetry-writer", ["main"])["main"]
    collector_module = load_component_modules("dataset-collector", ["main"])["main"]
    signals = (
        ["PowerCyl4"] if run_id.endswith("_2")
        else ["IMEPn1", "IMEPn2", "IMEPn3", "IMEPn4", "MFB50_4", "PMax4", "PowerCyl4"]
    )
    profiles = [
        replay.SignalProfile("short", signal, 6, 32, 19, 2, 2, 0, 7200)
        for signal in signals
    ]
    config = replay.StreamConfig(mode, run_id, 1000, 1, False, 7)
    connection = MagicMock()
    connection.execute.return_value.fetchall.return_value = [("Bore", "200")]
    collector = collector_module.DatasetCollector()
    collector.hello({"protocol": 1, "name": "dataset", "node_id": "agent-1"})
    with (
        patch.object(replay.psycopg, "connect") as connect,
        patch.object(replay, "_profiles", return_value=profiles),
        patch.object(replay, "_series_rows", side_effect=lambda cursor, run, source, signal: iter([
            (source, signal, 0, 6), (source, signal, 7200, 32),
        ])),
    ):
        connect.return_value.__enter__.return_value = connection
        records = list(
            replay.synthetic_records(config) if mode == "synthetic" else replay.real_records(config)
        )
    assert len(records) == 2 * len(signals)
    for index, record in enumerate(records):
        message = {
            **record, "timestamp": 1761200000, "timestamp_ns": 1761200000000000000 + index,
        }
        assert collector.ingest_json(json.dumps(message))
        point, = writer._to_points(message, writer.DATASET_KAFKA_TOPIC)
        line = point.to_line_protocol()
        assert f"run_id={run_id}" in line
        assert f"signal={record['signal']}" in line
        assert f"origin={mode}" in line
        assert f"t_source={record['t_source']:g}" in line
        assert line.endswith(str(message["timestamp_ns"]))
    samples = collector.collect({"seq": 1, "now_unix": 1761200000})["samples"]
    assert {sample["metric_type"] for sample in samples} == set(signals)
    for sample in samples:
        assert sample["subject"] == f"engine:{run_id}"
        assert sample["unit"] == "dataset-native"
        assert sample["range"] == [6, 32]
        assert 6 <= sample["value"] <= 32
        assert sample["labels"]["origin"] == mode
    assert collector.status()["dropped"] == 0


def test_live_pontos_arrivals_reach_writer_and_windowed_collector(monkeypatch):
    pontos = load_component_modules("pontos-bridge", ["main"])["main"]
    writer = load_component_modules("telemetry-writer", ["main"])["main"]
    collector_module = load_component_modules("dataset-collector", ["main"])["main"]
    monkeypatch.setattr(collector_module.time, "time", lambda: 1761200001)
    collector = collector_module.DatasetCollector()
    collector.hello({"protocol": 1, "name": "dataset", "node_id": "agent-1", "interval_ms": 1000})
    config = pontos.BridgeConfig()
    for vessel, index, timestamp, value in [
        ("imo_9007128", "1", 1761200000, 10),
        ("imo_9007128", "1", 1761200000.5, 20),
        ("imo_9007128", "2", 1761200000, 40),
        ("mmsi_265585310", "1", 1761200000, 80),
    ]:
        message = pontos.map_message(
            config, f"PONTOS_EGRESS/{vessel}/enginemain_power_kw/{index}",
            json.dumps({"timestamp": timestamp, "value": str(value)}).encode(),
            1761200001,
        )
        assert collector.ingest_json(json.dumps(message))
        point, = writer._to_points(message, writer.DATASET_KAFKA_TOPIC)
        line = point.to_line_protocol()
        assert "origin=live" in line
        assert f"vessel_id={vessel}" in line
        assert f"parameter_id=enginemain_power_kw_{index}" in line
        assert 'data_license="ODbL-1.0 / DbCL-1.0"' in line
        assert line.endswith(str(message["timestamp_ns"]))
    samples = collector.collect({"seq": 1, "now_unix": 1761200001})["samples"]
    assert {(sample["subject"], sample["metric_type"]): sample["value"] for sample in samples} == {
        ("vessel:imo_9007128", "pontos.enginemain_power_kw.i_1"): 15,
        ("vessel:imo_9007128", "pontos.enginemain_power_kw.i_2"): 40,
        ("vessel:mmsi_265585310", "pontos.enginemain_power_kw.i_1"): 80,
    }
    assert all(sample["unit"] == "kW" and "range" not in sample for sample in samples)
    assert all(sample["timestamp_unix"] == 1761200001 for sample in samples)
    assert collector.collect({"seq": 2, "now_unix": 1761200002})["samples"] == []
    assert collector.status()["dropped"] == 0


def test_live_pontos_percent_retains_declared_range(monkeypatch):
    pontos = load_component_modules("pontos-bridge", ["main"])["main"]
    collector_module = load_component_modules("dataset-collector", ["main"])["main"]
    monkeypatch.setattr(collector_module.time, "time", lambda: 1761200001)
    collector = collector_module.DatasetCollector()
    collector.hello({"protocol": 1, "name": "dataset", "node_id": "agent-1"})
    message = pontos.map_message(
        pontos.BridgeConfig(), "PONTOS_EGRESS/imo_9007128/battery_soc_pct/1",
        b'{"timestamp":1761200000,"value":75}', 1761200000,
    )
    assert collector.ingest_json(json.dumps(message))
    sample, = collector.collect({"seq": 1, "now_unix": 1761200001})["samples"]
    assert sample["value"] == 75
    assert sample["unit"] == "percent"
    assert sample["range"] == [0, 100]
