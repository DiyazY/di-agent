from collections import namedtuple
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest
from conftest import load_component_modules


@pytest.fixture
def sync_modules():
    return {"sync": load_component_modules("telemetry-cloud-sync", ["main"])["main"]}


Record = namedtuple("Record", ["topic", "key", "value"])


def test_send_to_eventhub(sync_modules, monkeypatch):
    sync_mod = sync_modules["sync"]
    mock_producer = MagicMock()
    mock_future = MagicMock()
    mock_producer.send.return_value = mock_future

    monkeypatch.setattr(
        sync_mod,
        "EVENTHUB_TOPIC_MAP",
        {"genset.telemetry": "eh-genset"},
    )

    record = Record(topic="genset.telemetry", key=b"g1", value=b'{"power": 100}')
    target = sync_mod._send_to_eventhub(mock_producer, record)

    assert target == "eh-genset"
    mock_producer.send.assert_called_once_with("eh-genset", key=b"g1", value=b'{"power": 100}')
    mock_future.get.assert_called_once_with(timeout=30)


def test_send_to_eventhub_fallback(sync_modules, monkeypatch):
    sync_mod = sync_modules["sync"]
    mock_producer = MagicMock()
    mock_future = MagicMock()
    mock_producer.send.return_value = mock_future

    monkeypatch.setattr(sync_mod, "EVENTHUB_TOPIC_MAP", {})

    record = Record(topic="propulsion.telemetry", key=b"p1", value=b'{"speed": 10}')
    target = sync_mod._send_to_eventhub(mock_producer, record)

    assert target == "propulsion.telemetry"
    mock_producer.send.assert_called_once_with("propulsion.telemetry", key=b"p1", value=b'{"speed": 10}')


def test_make_eventhub_producer_missing_creds(sync_modules, monkeypatch):
    sync_mod = sync_modules["sync"]
    monkeypatch.setattr(sync_mod, "EVENTHUB_NAMESPACE_FQDN", "")
    monkeypatch.setattr(sync_mod, "EVENTHUB_CONNECTION_STRING", "")

    with pytest.raises(RuntimeError, match="EVENTHUB_NAMESPACE_FQDN and EVENTHUB_CONNECTION_STRING"):
        sync_mod._make_eventhub_producer()
