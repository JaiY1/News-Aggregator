import sqlite3
import json
import os
import secrets

DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "newsletter.db")


def get_conn():
    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    return conn


def init_db():
    conn = get_conn()

    # Users table — one row per subscriber
    conn.execute("""
        CREATE TABLE IF NOT EXISTS users (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            phone_number TEXT UNIQUE,
            name TEXT,
            interests TEXT DEFAULT '[]',
            sources TEXT DEFAULT '[]',
            subreddits TEXT DEFAULT '[]',
            location TEXT,
            active INTEGER DEFAULT 1,
            created_at TEXT DEFAULT (datetime('now'))
        )
    """)

    # Add image_url column if not exists (migration)
    try:
        conn.execute("ALTER TABLE articles ADD COLUMN image_url TEXT")
        conn.commit()
    except Exception:
        pass

    # Add source_url column if not exists (migration) — the raw feed URL
    # (a Google News redirect, before decoding) so re-scrapes can recognize
    # an already-known article without re-decoding/re-fetching it.
    try:
        conn.execute("ALTER TABLE articles ADD COLUMN source_url TEXT")
        conn.commit()
    except Exception:
        pass

    # Add token column if not exists (migration) — the opaque identifier used
    # in shareable feed links now that signup no longer requires a phone
    # number. phone_number stays in the schema, unused, for future SMS work.
    # SQLite can't add a UNIQUE column via ALTER TABLE, so uniqueness is
    # enforced with a separate index instead.
    try:
        conn.execute("ALTER TABLE users ADD COLUMN token TEXT")
        conn.commit()
    except Exception:
        pass
    conn.execute("CREATE UNIQUE INDEX IF NOT EXISTS idx_users_token ON users(token)")

    # Articles table — deduped by URL
    conn.execute("""
        CREATE TABLE IF NOT EXISTS articles (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            url TEXT UNIQUE,
            title TEXT,
            source TEXT,
            category TEXT,
            excerpt TEXT,
            summary TEXT,
            image_url TEXT,
            source_url TEXT,
            published_at TEXT,
            scraped_at TEXT DEFAULT (datetime('now'))
        )
    """)

    conn.execute("CREATE INDEX IF NOT EXISTS idx_articles_source_url ON articles(source_url)")

    # Tracks which articles were included in each user's digest
    conn.execute("""
        CREATE TABLE IF NOT EXISTS user_digests (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            user_id INTEGER,
            article_id INTEGER,
            sent_at TEXT DEFAULT (datetime('now')),
            UNIQUE(user_id, article_id)
        )
    """)

    # Cost tracking table
    conn.execute("""
        CREATE TABLE IF NOT EXISTS api_costs (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            service TEXT,
            operation TEXT,
            units REAL,
            cost_usd REAL,
            logged_at TEXT DEFAULT (datetime('now'))
        )
    """)

    conn.commit()
    conn.close()
    print("DB initialized.")


# --- User functions ---

def create_user(name: str = None) -> dict:
    """Create a user identified by an opaque token (their shareable feed link)
    instead of a phone number. Returns the created user dict."""
    token = secrets.token_urlsafe(8)
    conn = get_conn()
    conn.execute(
        "INSERT INTO users (token, name) VALUES (?, ?)",
        [token, name]
    )
    conn.commit()
    conn.close()
    return get_user(token)


def get_user(token: str):
    conn = get_conn()
    row = conn.execute(
        "SELECT * FROM users WHERE token = ?", [token]
    ).fetchone()
    conn.close()
    if not row:
        return None
    user = dict(row)
    user["interests"] = json.loads(user["interests"])
    user["sources"] = json.loads(user["sources"])
    user["subreddits"] = json.loads(user["subreddits"])
    return user


def get_all_users():
    conn = get_conn()
    rows = conn.execute("SELECT * FROM users WHERE active = 1").fetchall()
    conn.close()
    users = []
    for row in rows:
        u = dict(row)
        u["interests"] = json.loads(u["interests"])
        u["sources"] = json.loads(u["sources"])
        u["subreddits"] = json.loads(u["subreddits"])
        users.append(u)
    return users


def update_user(token: str, **kwargs):
    conn = get_conn()
    for key, value in kwargs.items():
        if isinstance(value, list):
            value = json.dumps(value)
        conn.execute(
            f"UPDATE users SET {key} = ? WHERE token = ?",
            [value, token]
        )
    conn.commit()
    conn.close()


# --- Article functions ---

def save_article(article: dict):
    conn = get_conn()
    try:
        cursor = conn.execute("""
            INSERT OR IGNORE INTO articles
                (url, title, source, category, excerpt, summary, image_url, source_url, published_at)
            VALUES
                (:url, :title, :source, :category, :excerpt, :summary, :image_url, :source_url, :published_at)
        """, {**article, "image_url": article.get("image_url"), "source_url": article.get("source_url")})
        # Patch image_url or summary if they were missing before
        if article.get("image_url"):
            conn.execute(
                "UPDATE articles SET image_url = ? WHERE url = ? AND (image_url IS NULL OR image_url = '')",
                [article["image_url"], article["url"]]
            )
        conn.commit()
        article_id = cursor.lastrowid if cursor.lastrowid else None
    except Exception:
        article_id = None
    conn.close()
    return article_id


def get_articles_by_source_urls(source_urls: list[str]) -> dict:
    """Look up already-scraped articles by their raw feed URL (pre-decode/pre-enrichment),
    so a re-scrape can skip re-fetching body/image/decoding for ones we already have."""
    if not source_urls:
        return {}
    conn = get_conn()
    placeholders = ",".join("?" for _ in source_urls)
    rows = conn.execute(
        f"SELECT * FROM articles WHERE source_url IN ({placeholders})", source_urls
    ).fetchall()
    conn.close()
    return {r["source_url"]: dict(r) for r in rows}


def save_summary(url: str, summary: str):
    """Persist a generated summary back to the article row."""
    conn = get_conn()
    conn.execute("UPDATE articles SET summary = ? WHERE url = ?", [summary, url])
    conn.commit()
    conn.close()


def get_articles_for_user(user_id: int, interests: list[str], limit: int = 20):
    conn = get_conn()
    # Get articles not yet sent to this user, matching their interests
    placeholders = " OR ".join(["LOWER(category) LIKE ?" for _ in interests])
    params = [f"%{i.lower()}%" for i in interests] + [user_id, limit]
    rows = conn.execute(f"""
        SELECT a.* FROM articles a
        WHERE ({placeholders})
        AND a.id NOT IN (
            SELECT article_id FROM user_digests WHERE user_id = ?
        )
        ORDER BY a.scraped_at DESC
        LIMIT ?
    """, params).fetchall()
    conn.close()
    return [dict(r) for r in rows]


def mark_articles_sent(user_id: int, article_ids: list[int]):
    conn = get_conn()
    conn.executemany(
        "INSERT OR IGNORE INTO user_digests (user_id, article_id) VALUES (?, ?)",
        [(user_id, aid) for aid in article_ids]
    )
    conn.commit()
    conn.close()


# --- Cost tracking ---

def log_cost(service: str, operation: str, units: float, cost_usd: float):
    conn = get_conn()
    conn.execute(
        "INSERT INTO api_costs (service, operation, units, cost_usd) VALUES (?, ?, ?, ?)",
        [service, operation, units, cost_usd]
    )
    conn.commit()
    conn.close()


def get_cost_summary():
    conn = get_conn()
    rows = conn.execute("""
        SELECT service, SUM(cost_usd) as total,
               DATE(logged_at) as date
        FROM api_costs
        GROUP BY service, DATE(logged_at)
        ORDER BY date DESC
    """).fetchall()
    conn.close()
    return [dict(r) for r in rows]
