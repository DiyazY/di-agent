FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY shore_power.py controller.py main.py .

ENV KAFKA_BROKERS=localhost:9092 \
    KAFKA_TOPIC=shore_power.telemetry \
    STEP_INTERVAL_S=1 \
    RAMP_RATE_PER_S=0.1 \
    RATED_POWER_KW=1000 \
    BATTERY_URL=http://battery:8000

EXPOSE 8000

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
