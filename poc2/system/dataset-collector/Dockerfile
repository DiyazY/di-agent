FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py .

ENV KAFKA_BROKERS=kafka:9092 \
    KAFKA_TOPIC=dataset.telemetry \
    KAFKA_GROUP_ID=dataset-collector

EXPOSE 8000

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000", "--timeout-keep-alive", "75"]
