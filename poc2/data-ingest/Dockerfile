FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY ingest.py .

ENV DATA_DIR=/data \
    PGHOST=postgres \
    PGPORT=5432 \
    PGDATABASE=engine

CMD ["python", "ingest.py"]
