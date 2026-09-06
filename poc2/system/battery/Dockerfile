FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY battery.py controller.py main.py .

ENV KAFKA_BROKERS=localhost:9092 \
    KAFKA_TOPIC=battery.telemetry \
    STEP_INTERVAL_S=1 \
    RAMP_RATE_PER_S=0.05 \
    INITIAL_SOC=0.8 \
    PREDICTION_EMA_ALPHA=0.1 \
    PREDICTION_MIN_SOC_RATE_PER_HR=0.001

EXPOSE 8000

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
