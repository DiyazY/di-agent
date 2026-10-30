from collections import namedtuple
from pathlib import Path
from unittest.mock import MagicMock, call, patch

import pytest
from kafka import OffsetAndMetadata, TopicPartition
from conftest import load_component_modules


@pytest.fixture
def sync_modules():
    return {"sync": load_component_modules("telemetry-cloud-sync", ["main"])["main"]}


Record = namedtuple("Record", ["topic", "key", "value", "partition", "offset"], defaults=[0, 0])


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


def test_sync_records_retries_before_committing(sync_modules, monkeypatch):
    sync_mod = sync_modules["sync"]
    record = Record(topic="genset.telemetry", key=b"g1", value=b"{}")
    consumer = MagicMock()
    consumer.__iter__.return_value = iter([record])
    attempts = []

    def send(_record):
        attempts.append(_record)
        if len(attempts) == 1:
            raise TimeoutError("cloud unavailable")

    monkeypatch.setattr(sync_mod.time, "sleep", lambda _seconds: None)
    stats = sync_mod._sync_records(
        consumer,
        send,
        retry_backoff_s=0,
        log_every_n_messages=0,
    )

    assert stats == {"seen": 1, "synced": 1, "failed": 1}
    assert attempts == [record, record]
    consumer.commit.assert_called_once_with({
        TopicPartition(record.topic, record.partition): OffsetAndMetadata(record.offset + 1, ""),
    })


def test_sync_records_drops_permanent_error_and_advances(sync_modules):
    sync_mod = sync_modules["sync"]
    record = Record(topic="genset.telemetry", key=b"g1", value=b"{}")
    consumer = MagicMock()
    consumer.__iter__.return_value = iter([record])

    class PermanentError(Exception):
        is_transient = False

    with patch.object(sync_mod.time, "sleep") as sleep:
        stats = sync_mod._sync_records(
            consumer,
            lambda _record: (_ for _ in ()).throw(PermanentError("unauthorized")),
            retry_backoff_s=0,
            log_every_n_messages=0,
        )

    assert stats == {"seen": 1, "synced": 0, "failed": 1}
    consumer.commit.assert_called_once_with({
        TopicPartition(record.topic, record.partition): OffsetAndMetadata(record.offset + 1, ""),
    })
    sleep.assert_not_called()


def test_sync_records_commits_only_each_processed_record(sync_modules):
    sync_mod = sync_modules["sync"]
    records = [
        Record("genset.telemetry", b"g1", b"{}", 0, 3),
        Record("genset.telemetry", b"g1", b"{}", 0, 4),
    ]
    consumer = MagicMock()
    consumer.__iter__.return_value = iter(records)
    expected_commits = [
        call({TopicPartition(record.topic, record.partition): OffsetAndMetadata(record.offset + 1, "")})
        for record in records
    ]

    commits_at_send = []

    def send(record):
        commits_at_send.append(list(consumer.commit.call_args_list))

    sync_mod._sync_records(consumer, send, retry_backoff_s=0, log_every_n_messages=0)

    assert commits_at_send == [[], expected_commits[:1]]
    assert consumer.commit.call_args_list == expected_commits


def test_consumer_rejects_empty_topic_list(sync_modules):
    with pytest.raises(RuntimeError, match="no Kafka topics"):
        sync_modules["sync"]._make_consumer([])


def test_iothub_subscription_includes_only_configured_sink_topics(sync_modules):
    sync_mod = sync_modules["sync"]
    topics = ["genset.telemetry", "battery.telemetry", "propulsion.telemetry"]
    clients = {"battery.telemetry": object()}

    assert sync_mod._topics_with_sinks(topics, clients) == ["battery.telemetry"]
