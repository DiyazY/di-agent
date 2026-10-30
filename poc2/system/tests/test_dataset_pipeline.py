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
