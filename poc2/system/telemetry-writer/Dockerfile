FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py .

ENV KAFKA_BROKERS=localhost:9092 \
    GENSET_KAFKA_TOPIC=genset.telemetry \
    PROPULSION_KAFKA_TOPIC=propulsion.telemetry \
    BATTERY_KAFKA_TOPIC=battery.telemetry \
    AUXLOAD_KAFKA_TOPIC=auxload.telemetry \
    KAFKA_GROUP_ID=telemetry-writer \
    INFLUXDB_URL=http://localhost:8086 \
    INFLUXDB_ORG=di-agent \
    INFLUXDB_BUCKET=telemetry \
    INFLUXDB_TOKEN=

CMD ["python", "main.py"]
