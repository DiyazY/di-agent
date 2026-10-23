FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY switchboard.py controller.py main.py modbus_client.py .

ENV KAFKA_BROKERS=localhost:9092 \
    KAFKA_TOPIC=switchboard.telemetry \
    REQUEST_KAFKA_TOPIC=switchboard.requests \
    STEP_INTERVAL_S=1 \
    GENSET_MODBUS_TARGETS= \
    BATTERY_MODBUS_TARGETS= \
    MODBUS_POLL_INTERVAL_S=2

EXPOSE 8000

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
