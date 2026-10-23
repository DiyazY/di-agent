FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py .

# Unbuffered stdout: print() otherwise sits in a block buffer and never
# shows up in `kubectl logs` until enough output accumulates.
ENV PYTHONUNBUFFERED=1 \
    KAFKA_BROKERS=localhost:9092 \
    SYNC_KAFKA_TOPICS=genset.telemetry,propulsion.telemetry,battery.telemetry,auxload.telemetry,shore_power.telemetry,switchboard.telemetry \
    KAFKA_GROUP_ID=telemetry-cloud-sync \
    SYNC_TARGET=eventhub \
    EVENTHUB_NAMESPACE_FQDN= \
    EVENTHUB_CONNECTION_STRING= \
    EVENTHUB_TOPIC_MAP={} \
    IOTHUB_TOPIC_ENV_MAP={} \
    RETRY_BACKOFF_S=5 \
    LOG_EVERY_N_MESSAGES=50

CMD ["python", "main.py"]
