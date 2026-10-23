import time
from pathlib import Path

import pytest
from conftest import load_component_modules


@pytest.fixture
def writer_modules():
    return {"writer": load_component_modules("telemetry-writer", ["main"])["main"]}


def test_normalize_message(writer_modules):
    norm = writer_modules["writer"]._normalize_message

    # Flat dict
    flat = {"genset_id": "g1", "power_kw": 1000.0}
    assert norm(flat) == flat

    # Envelope with payload
    wrapped = {
        "source_id": "g1",
        "source_type": "genset",
        "event": "genset.telemetry",
        "timestamp": 1234567.89,
        "payload": {"genset_id": "g1", "power_kw": 1000.0},
    }
    unwrapped = norm(wrapped)
    assert unwrapped["genset_id"] == "g1"
    assert unwrapped["power_kw"] == 1000.0
    assert unwrapped["source_id"] == "g1"
    assert unwrapped["timestamp"] == 1234567.89

    with pytest.raises(TypeError):
        norm("invalid string")


def test_to_points_all_components(writer_modules):
    to_points = writer_modules["writer"]._to_points
    ts = time.time()

    # 1. Genset
    msg_genset = {"genset_id": "genset-1", "power_kw": 2500.0, "load_ratio": 0.6, "timestamp": ts}
    pts = to_points(msg_genset)
    assert len(pts) == 1

    # 2. Propulsion
    msg_prop = {"propulsion_id": "prop-1", "power_output_kw": 3000.0, "load_ratio": 0.5, "timestamp": ts}
    pts = to_points(msg_prop)
    assert len(pts) == 1

    # 3. Battery
    msg_battery = {
        "battery_id": "batt-1",
        "soc": 0.85,
        "power_kw": 500.0,
        "cycle_type": "discharge",
        "timestamp": ts,
    }
    pts = to_points(msg_battery)
    assert len(pts) == 1

    # 4. Shore Power
    msg_shore = {"shore_power_id": "sp-1", "power_kw": 800.0, "power_ratio": 0.8, "timestamp": ts}
    pts = to_points(msg_shore)
    assert len(pts) == 1

    # 5. Aux Load
    msg_aux = {"auxload_id": "aux-1", "power_output_kw": 350.0, "load_ratio": 0.5, "timestamp": ts}
    pts = to_points(msg_aux)
    assert len(pts) == 1

    # 6. Switchboard (produces 2 points: switchboard_telemetry and switchboard_aggregate)
    msg_swb = {
        "consumer_id": "prop-1",
        "requested_power_kw": 3000.0,
        "allocated_power_kw": 3000.0,
        "available_supply_kw": 6000.0,
        "switchboard_id": "swb-1",
        "timestamp": ts,
    }
    pts = to_points(msg_swb)
    assert len(pts) == 2

    # 7. Navigation
    msg_nav = {"gnss_id": "gnss-1", "latitude_deg": 1.25, "longitude_deg": 103.8, "timestamp": ts}
    pts = to_points(msg_nav)
    assert len(pts) == 1


def test_to_points_invalid(writer_modules):
    to_points = writer_modules["writer"]._to_points

    # Missing ID tag
    with pytest.raises(KeyError, match="known id tags"):
        to_points({"unknown_id": "test", "timestamp": 12345})

    # Missing timestamp
    with pytest.raises(KeyError, match="timestamp"):
        to_points({"genset_id": "genset-1", "power_kw": 100})


def test_dataset_topic_writes_generic_signal_point(writer_modules):
    to_points = writer_modules["writer"]._to_points
    message = {
        "run_id": "test-run",
        "source": "short",
        "signal": "Injection",
        "value": -11.5,
        "t_source": 7200.0,
        "origin": "real",
        "timestamp": 123.0,
        "timestamp_ns": 123_000_000_001,
    }

    points = to_points(message, "dataset.telemetry")

    assert len(points) == 1
    line = points[0].to_line_protocol()
    assert line.startswith(
        "dataset_timeseries,origin=real,run_id=test-run,signal=Injection,source=short "
    )
    assert "t_source=7200" in line
    assert "value=-11.5" in line
    assert line.endswith("123000000001")
