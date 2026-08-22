FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY propulsion.py controller.py main.py .

ENV KAFKA_BROKERS=localhost:9092 \
    KAFKA_TOPIC=propulsion.telemetry \
    STEP_INTERVAL_S=1 \
    RAMP_RATE_PER_S=0.05

EXPOSE 8000

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
