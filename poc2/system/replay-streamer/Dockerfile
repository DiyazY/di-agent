FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py .

ENV PGHOST=postgres \
    PGPORT=5432 \
    PGDATABASE=engine \
    KAFKA_BROKERS=kafka:9092 \
    KAFKA_TOPIC=dataset.telemetry \
    MODE=synthetic \
    RUN_ID=1000rpm_150kW_0001 \
    SOURCE=short \
    INTERVAL_MS=1000 \
    POINTS_PER_INTERVAL=10 \
    LOOP=true \
    READ_AHEAD_ROWS=1000 \
    RANDOM_SEED=1

EXPOSE 8000

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
