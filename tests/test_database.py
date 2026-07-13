def test_init_db_creates_tables(db):
    conn = db.get_conn()
    tables = {r["name"] for r in conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table'"
    ).fetchall()}
    conn.close()
    assert {"users", "articles", "user_digests", "api_costs"} <= tables


def test_create_and_get_user(db):
    created = db.create_user("Jai")
    user = db.get_user(created["token"])
    assert user["name"] == "Jai"
    assert user["interests"] == []
    assert user["active"] == 1


def test_create_user_generates_unique_tokens(db):
    a = db.create_user("Jai")
    b = db.create_user("Jai")  # same name, different person — token must differ
    assert a["token"] != b["token"]


def test_get_user_missing_returns_none(db):
    assert db.get_user("not-a-real-token") is None


def test_update_user_sets_list_fields(db):
    created = db.create_user("Jai")
    db.update_user(created["token"], interests=["nba", "ai"], name="Jai Y")
    user = db.get_user(created["token"])
    assert user["interests"] == ["nba", "ai"]
    assert user["name"] == "Jai Y"


def test_get_all_users_only_active(db):
    a = db.create_user("A")
    b = db.create_user("B")
    db.update_user(b["token"], active=0)
    users = db.get_all_users()
    tokens = {u["token"] for u in users}
    assert tokens == {a["token"]}


def _article(url="https://example.com/a", category="nba", source_url=None):
    return {
        "url": url,
        "source_url": source_url or url,
        "title": "Test Article",
        "source": "Example",
        "category": category,
        "excerpt": "Some excerpt",
        "summary": None,
        "image_url": None,
        "published_at": "2026-01-01",
    }


def test_save_article_dedups_by_url(db):
    first_id = db.save_article(_article())
    second_id = db.save_article(_article())
    assert first_id is not None
    conn = db.get_conn()
    count = conn.execute("SELECT COUNT(*) as c FROM articles").fetchone()["c"]
    conn.close()
    assert count == 1
    assert second_id is None  # INSERT OR IGNORE — no new row, no lastrowid


def test_get_articles_by_source_urls_finds_known_and_ignores_unknown(db):
    db.save_article(_article("https://real.com/resolved-1", source_url="https://news.google.com/rss/articles/AAA"))
    db.save_article(_article("https://real.com/resolved-2", source_url="https://news.google.com/rss/articles/BBB"))

    found = db.get_articles_by_source_urls([
        "https://news.google.com/rss/articles/AAA",
        "https://news.google.com/rss/articles/ZZZ",  # never scraped
    ])

    assert set(found.keys()) == {"https://news.google.com/rss/articles/AAA"}
    assert found["https://news.google.com/rss/articles/AAA"]["url"] == "https://real.com/resolved-1"


def test_get_articles_by_source_urls_empty_input(db):
    assert db.get_articles_by_source_urls([]) == {}


def test_get_articles_for_user_filters_by_interest_and_excludes_sent(db):
    user = db.create_user("Jai")

    db.save_article(_article("https://example.com/nba1", "nba"))
    db.save_article(_article("https://example.com/ai1", "ai"))

    unsent = db.get_articles_for_user(user["id"], ["nba"])
    assert len(unsent) == 1
    assert unsent[0]["category"] == "nba"

    db.mark_articles_sent(user["id"], [a["id"] for a in unsent])
    unsent_again = db.get_articles_for_user(user["id"], ["nba"])
    assert unsent_again == []


def test_log_cost_and_summary(db):
    db.log_cost("claude-haiku", "summarize_article", 100, 0.0005)
    db.log_cost("claude-haiku", "summarize_article", 200, 0.001)
    summary = db.get_cost_summary()
    assert len(summary) == 1
    assert round(summary[0]["total"], 6) == round(0.0015, 6)
