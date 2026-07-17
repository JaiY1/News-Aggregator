FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /app

COPY requirements.txt .
RUN pip install -r requirements.txt

COPY . .

# All mutable data (SQLite DB) lives on a Railway Volume mounted at /data —
# configure that in the Railway dashboard, not here (Railway rejects a Docker
# VOLUME instruction and manages volumes itself).
ENV DATA_DIR=/data \
    PORT=8080
EXPOSE 8080

# Single worker: SQLite + in-process caches (_refresh_status, _briefing_cache)
# don't tolerate multiple worker processes. Threads share memory and comfortably
# handle a few concurrent users. Shell form so $PORT (Railway sets its own) expands.
CMD gunicorn api:app --bind 0.0.0.0:$PORT --workers 1 --threads 4 --timeout 120
