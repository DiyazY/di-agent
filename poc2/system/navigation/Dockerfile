FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY vessel.py sensors.py controller.py main.py nmea2000.py .

ENV KAFKA_BROKERS=localhost:9092 \
    KAFKA_TOPIC=navigation.telemetry \
    TICK_INTERVAL_S=0.2 \
    KAFKA_EVERY_N_TICKS=5 \
    N2K_HOST=0.0.0.0 \
    N2K_PORT=2597 \
    VESSEL_NAME=vessel-1 \
    INITIAL_SPEED_KNOTS=8 \
    NAV_TIME_SCALE=60

EXPOSE 8000 2597

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
