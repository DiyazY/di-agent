import json
import os
import time

from kafka import KafkaConsumer, KafkaProducer, OffsetAndMetadata, TopicPartition

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092").split(",")
KAFKA_TOPICS = [
    topic.strip()
    for topic in os.environ.get(
        "SYNC_KAFKA_TOPICS",
        "genset.telemetry,propulsion.telemetry,battery.telemetry,"
        "auxload.telemetry,shore_power.telemetry,switchboard.telemetry,dataset.telemetry",
    ).split(",")
    if topic.strip()
]
KAFKA_GROUP_ID = os.environ.get("KAFKA_GROUP_ID", "telemetry-cloud-sync")

# "eventhub" (default) mirrors to Azure Event Hubs over its Kafka-compatible
# endpoint. "iothub" sends each topic's messages as device-to-cloud
# telemetry through a per-service Azure IoT Hub device identity.
SYNC_TARGET = os.environ.get("SYNC_TARGET", "eventhub").strip().lower()

# --- Event Hubs sink: a second Kafka producer pointed at the Kafka-compatible
# endpoint, no Azure SDK required. ---

EVENTHUB_NAMESPACE_FQDN = os.environ.get("EVENTHUB_NAMESPACE_FQDN", "")
EVENTHUB_CONNECTION_STRING = os.environ.get("EVENTHUB_CONNECTION_STRING", "")
# Optional JSON mapping of local Kafka topic -> Event Hub name, e.g.
# {"genset.telemetry": "genset-telemetry"}. Falls back to the local topic
# name (an Event Hub with that exact name must exist in the namespace).
EVENTHUB_TOPIC_MAP = json.loads(os.environ.get("EVENTHUB_TOPIC_MAP", "{}"))

RETRY_BACKOFF_S = float(os.environ.get("RETRY_BACKOFF_S", "5"))
LOG_EVERY_N_MESSAGES = int(os.environ.get("LOG_EVERY_N_MESSAGES", "50"))


def _make_consumer(topics: list[str] | None = None) -> KafkaConsumer:
    subscribed_topics = KAFKA_TOPICS if topics is None else topics
    if not subscribed_topics:
        raise RuntimeError("no Kafka topics have configured cloud sinks")
    while True:
        try:
            return KafkaConsumer(
                *subscribed_topics,
                bootstrap_servers=KAFKA_BROKERS,
                group_id=KAFKA_GROUP_ID,
                # Raw passthrough: mirrored bytes are re-published as-is,
                # so telemetry-writer's schema stays the single source of truth.
                value_deserializer=lambda v: v,
                key_deserializer=lambda k: k,
                enable_auto_commit=False,
                auto_offset_reset="earliest",
            )
        except Exception as exc:  # noqa: BLE001 - local broker may not be up yet
            print(f"Kafka brokers {KAFKA_BROKERS} not available yet ({exc}), retrying in 5s ...")
            time.sleep(5)


def _make_eventhub_producer() -> KafkaProducer:
    if not EVENTHUB_NAMESPACE_FQDN or not EVENTHUB_CONNECTION_STRING:
        raise RuntimeError("EVENTHUB_NAMESPACE_FQDN and EVENTHUB_CONNECTION_STRING are required")
    while True:
        try:
            return KafkaProducer(
                bootstrap_servers=[f"{EVENTHUB_NAMESPACE_FQDN}:9093"],
                security_protocol="SASL_SSL",
                sasl_mechanism="PLAIN",
                sasl_plain_username="$ConnectionString",
                sasl_plain_password=EVENTHUB_CONNECTION_STRING,
                acks="all",
                retries=5,
                linger_ms=200,
            )
        except Exception as exc:  # noqa: BLE001 - cloud endpoint may be unreachable
            print(f"Event Hubs namespace {EVENTHUB_NAMESPACE_FQDN} not reachable yet ({exc}), retrying in 5s ...")
            time.sleep(5)


def _send_to_eventhub(producer: KafkaProducer, record) -> str:
    event_hub = EVENTHUB_TOPIC_MAP.get(record.topic, record.topic)
    future = producer.send(event_hub, key=record.key, value=record.value)
    future.get(timeout=30)
    return event_hub


# --- IoT Hub sink: one device identity per telemetry service, messages sent
# as device-to-cloud telemetry via the Azure IoT device SDK. ---

# JSON mapping of local Kafka topic -> name of the environment variable that
# holds that topic's device connection string, e.g.
# {"genset.telemetry": "IOTHUB_CONN_GENSET"}. Keeping the mapping indirect
# means the connection strings themselves never have to pass through JSON.
IOTHUB_TOPIC_ENV_MAP = json.loads(os.environ.get("IOTHUB_TOPIC_ENV_MAP", "{}"))


def _make_iothub_clients() -> dict:
    from azure.iot.device import IoTHubDeviceClient

    clients = {}
    for topic, env_name in IOTHUB_TOPIC_ENV_MAP.items():
        connection_string = os.environ.get(env_name, "")
        if not connection_string:
            print(f"No IoT Hub connection string in ${env_name} for topic {topic}; that topic will not be synced")
            continue
        client = IoTHubDeviceClient.create_from_connection_string(connection_string)
        client.connect()
        clients[topic] = client
    if not clients:
        raise RuntimeError("no IoT Hub device connection strings configured (IOTHUB_TOPIC_ENV_MAP)")
    return clients


def _topics_with_sinks(topics: list[str], clients: dict) -> list[str]:
    return [topic for topic in topics if topic in clients]


def _send_to_iothub(clients: dict, record) -> str:
    from azure.iot.device import Message

    client = clients.get(record.topic)
    if client is None:
        raise RuntimeError(f"no IoT Hub device configured for topic {record.topic}")
    message = Message(record.value)
    message.content_type = "application/json"
    message.content_encoding = "utf-8"
    message.custom_properties["source_topic"] = record.topic
    client.send_message(message)
    return record.topic


_PERMANENT_ERROR_NAMES = {
    "MessageSizeTooLargeError",
    "TopicAuthorizationFailedError",
    "AuthorizationFailedError",
    "CredentialError",
    "IoTHubClientError",
}


def _is_retryable_error(error: Exception) -> bool:
    transient = getattr(error, "is_transient", None)
    if transient is not None:
        return bool(transient)
    retriable = getattr(error, "retriable", None)
    if callable(retriable):
        return bool(retriable())
    return type(error).__name__ not in _PERMANENT_ERROR_NAMES


def _sync_records(consumer, send, *, retry_backoff_s: float, log_every_n_messages: int) -> dict:
    stats = {"seen": 0, "synced": 0, "failed": 0}
    for record in consumer:
        stats["seen"] += 1
        while True:
            try:
                send(record)
                stats["synced"] += 1
                break
            except Exception as exc:  # noqa: BLE001 - cloud SDK errors vary by sink
                stats["failed"] += 1
                if not _is_retryable_error(exc):
                    print(
                        f"Dropping non-retryable {SYNC_TARGET} message from "
                        f"{record.topic}: {exc}"
                    )
                    break
                print(
                    f"Failed to mirror message from {record.topic} to {SYNC_TARGET}: {exc}; "
                    f"retrying in {retry_backoff_s}s"
                )
                time.sleep(retry_backoff_s)
        consumer.commit({
            TopicPartition(record.topic, record.partition): OffsetAndMetadata(record.offset + 1, ""),
        })
        if log_every_n_messages and stats["seen"] % log_every_n_messages == 0:
            print(
                "telemetry-cloud-sync stats: "
                f"seen={stats['seen']} synced={stats['synced']} failed={stats['failed']}"
            )
    return stats


def main() -> None:
    if SYNC_TARGET == "iothub":
        sinks = _make_iothub_clients()
        topics = _topics_with_sinks(KAFKA_TOPICS, sinks)
        if not topics:
            for client in sinks.values():
                client.shutdown()
            raise RuntimeError("no configured IoT Hub sinks match SYNC_KAFKA_TOPICS")
        consumer = _make_consumer(topics)
        send = lambda record: _send_to_iothub(sinks, record)  # noqa: E731
        print(f"Mirroring {KAFKA_TOPICS} from {KAFKA_BROKERS} to Azure IoT Hub devices {list(sinks)}")
    elif SYNC_TARGET == "eventhub":
        sinks = _make_eventhub_producer()
        consumer = _make_consumer()
        send = lambda record: _send_to_eventhub(sinks, record)  # noqa: E731
        print(f"Mirroring {KAFKA_TOPICS} from {KAFKA_BROKERS} to Event Hubs namespace {EVENTHUB_NAMESPACE_FQDN}")
    else:
        raise RuntimeError(f"Unknown SYNC_TARGET {SYNC_TARGET!r}; expected 'eventhub' or 'iothub'")

    stats = {"seen": 0, "synced": 0, "failed": 0}
    try:
        stats = _sync_records(
            consumer,
            send,
            retry_backoff_s=RETRY_BACKOFF_S,
            log_every_n_messages=LOG_EVERY_N_MESSAGES,
        )
    finally:
        print(
            "telemetry-cloud-sync final stats: "
            f"seen={stats['seen']} synced={stats['synced']} failed={stats['failed']}"
        )
        if SYNC_TARGET == "iothub":
            for client in sinks.values():
                client.shutdown()
        else:
            sinks.close()
        consumer.close()


if __name__ == "__main__":
    main()
