web: gunicorn api:app --bind 0.0.0.0:$PORT
release: python -c "from database import init_db; init_db()"
