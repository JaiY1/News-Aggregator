web: gunicorn api:app --bind 0.0.0.0:$PORT --timeout 120 --workers 1 --threads 4
release: python -c "from database import init_db; init_db()"
