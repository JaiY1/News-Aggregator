import sys
import os

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import pytest
import database


@pytest.fixture
def db(tmp_path, monkeypatch):
    """Point database.py at a fresh throwaway SQLite file for this test."""
    db_path = str(tmp_path / "test_newsletter.db")
    monkeypatch.setattr(database, "DB_PATH", db_path)
    database.init_db()
    return database
