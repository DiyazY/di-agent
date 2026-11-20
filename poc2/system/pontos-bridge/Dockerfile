FROM python:3.11-slim

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY main.py .

ENV PONTOS_TOKEN_FILE=/var/run/secrets/pontos/access-token
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
