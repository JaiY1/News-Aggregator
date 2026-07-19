import pytest
import scraper


@pytest.fixture(autouse=True)
def no_db_cache(monkeypatch):
    """Tests exercise scrape_all's filtering logic, not the DB-cache lookup —
    default to 'nothing cached' unless a test overrides it."""
    monkeypatch.setattr(scraper, "get_articles_by_source_urls", lambda urls: {})


def test_domain_to_name_strips_www_and_tld():
    assert scraper.domain_to_name("https://www.startribune.com/some/path") == "Startribune"


def test_domain_to_name_falls_back_to_url_on_error():
    assert scraper.domain_to_name(None) is None


def test_is_paywalled_matches_domain_and_subdomain():
    assert scraper.is_paywalled("https://www.nytimes.com/2026/01/01/article")
    assert scraper.is_paywalled("https://cooking.nytimes.com/recipes/1")
    assert not scraper.is_paywalled("https://www.espn.com/nba/story")


def _fake_article(url, title="Title", source="Source", excerpt="", extra_negative=False):
    text = title + " " + excerpt
    if extra_negative:
        text += " parlay odds"
    return {
        "url": url,
        "source_url": url,
        "title": title,
        "source": source,
        "category": "nba",
        "excerpt": excerpt,
        "summary": None,
        "published_at": "2026-01-01",
        "image_url": None,
    }


def test_scrape_all_dedupes_and_enriches(monkeypatch):
    articles = [
        _fake_article("https://a.com/1", source="A", title="Story One"),
        _fake_article("https://a.com/1", source="A", title="Story One"),  # duplicate URL
        _fake_article("https://b.com/2", source="B", title="Story Two"),
    ]

    monkeypatch.setattr(scraper, "scrape_rss", lambda category, urls: articles)
    monkeypatch.setattr(scraper, "enrich_article", lambda a, is_google: a)

    result = scraper.scrape_all(["nba"])
    urls = {a["url"] for a in result}
    assert urls == {"https://a.com/1", "https://b.com/2"}


def test_scrape_all_drops_paywalled_articles(monkeypatch):
    articles = [_fake_article("https://a.com/1", source="A")]

    def fake_enrich(article, is_google):
        article["_paywalled"] = True
        return article

    monkeypatch.setattr(scraper, "scrape_rss", lambda category, urls: articles)
    monkeypatch.setattr(scraper, "enrich_article", fake_enrich)

    result = scraper.scrape_all(["nba"])
    assert result == []


def test_scrape_all_caps_two_articles_per_source_per_category(monkeypatch):
    headlines = [
        "Warriors clinch playoff spot with buzzer beater",
        "Celtics fire head coach after early exit",
        "Nuggets extend winning streak to ten games",
        "Heat trade for veteran point guard before deadline",
        "Bucks announce new arena naming rights deal",
    ]
    articles = [
        _fake_article(f"https://a.com/{i}", source="A", title=headlines[i]) for i in range(5)
    ]

    monkeypatch.setattr(scraper, "scrape_rss", lambda category, urls: articles)
    monkeypatch.setattr(scraper, "enrich_article", lambda a, is_google: a)

    result = scraper.scrape_all(["nba"])
    assert len(result) == 2


def test_scrape_all_applies_negative_keywords_for_timberwolves(monkeypatch):
    articles = [
        _fake_article("https://a.com/1", title="Wolves win big"),
        _fake_article("https://a.com/2", title="Best parlay odds for Wolves game"),
    ]

    monkeypatch.setattr(scraper, "scrape_rss", lambda category, urls: articles)
    monkeypatch.setattr(scraper, "enrich_article", lambda a, is_google: a)

    result = scraper.scrape_all(["timberwolves"])
    titles = [a["title"] for a in result]
    assert "Wolves win big" in titles
    assert not any("parlay" in t.lower() for t in titles)


def test_scrape_all_drops_near_duplicate_titles(monkeypatch):
    articles = [
        _fake_article("https://a.com/1", source="A", title="Lakers beat Celtics in overtime thriller to close out series"),
        _fake_article("https://b.com/2", source="B", title="Lakers beat Celtics in overtime thriller, close out series 4-2"),
        _fake_article("https://c.com/3", source="C", title="Celtics extend star forward with record contract extension"),
    ]

    monkeypatch.setattr(scraper, "scrape_rss", lambda category, urls: articles)
    monkeypatch.setattr(scraper, "enrich_article", lambda a, is_google: a)

    result = scraper.scrape_all(["nba"])
    titles = [a["title"] for a in result]
    assert len(titles) == 2  # the reworded repost is dropped, the distinct story stays
    assert "Celtics extend star forward with record contract extension" in titles


def test_scrape_all_reuses_cached_articles_without_enriching(monkeypatch):
    articles = [
        _fake_article("https://a.com/cached", source="A", title="Cached Story"),
        _fake_article("https://a.com/new", source="A", title="Brand New Story"),
    ]
    enriched_urls = []

    def fake_enrich(article, is_google):
        enriched_urls.append(article["url"])
        return article

    def fake_lookup(source_urls):
        assert set(source_urls) == {"https://a.com/cached", "https://a.com/new"}
        return {
            "https://a.com/cached": {
                "url": "https://a.com/cached-resolved",
                "title": "Cached Title",
                "source": "A",
                "excerpt": "cached excerpt",
                "summary": "cached summary",
                "image_url": "https://a.com/cached.png",
                "published_at": "2026-01-01",
            }
        }

    monkeypatch.setattr(scraper, "scrape_rss", lambda category, urls: articles)
    monkeypatch.setattr(scraper, "enrich_article", fake_enrich)
    monkeypatch.setattr(scraper, "get_articles_by_source_urls", fake_lookup)

    result = scraper.scrape_all(["nba"])

    # The cached article is reused from the DB row (resolved URL, stored summary)
    # without ever calling enrich_article on it.
    assert enriched_urls == ["https://a.com/new"]
    cached_result = next(a for a in result if a["source_url"] == "https://a.com/cached")
    assert cached_result["url"] == "https://a.com/cached-resolved"
    assert cached_result["summary"] == "cached summary"


def _struct_time_days_ago(days):
    import time
    from datetime import datetime, timedelta, timezone
    dt = datetime.now(timezone.utc) - timedelta(days=days)
    return dt.timetuple()


class _FakeEntry(dict):
    """feedparser entries are dict-like with attribute access; a plain dict
    with .get() covers everything scrape_rss reads off an entry."""
    pass


class _FakeFeed:
    def __init__(self, entries):
        self.entries = entries
        self.feed = {"title": "Fake Feed"}


def test_parse_published_returns_none_when_missing():
    assert scraper._parse_published({}) is None


def test_parse_published_converts_struct_time_to_utc_datetime():
    entry = {"published_parsed": _struct_time_days_ago(1)}
    dt = scraper._parse_published(entry)
    assert dt is not None
    assert dt.tzinfo is not None


def test_scrape_rss_drops_articles_older_than_max_age(monkeypatch):
    entries = [
        _FakeEntry(title="Fresh story", link="https://a.com/fresh",
                   published_parsed=_struct_time_days_ago(1)),
        _FakeEntry(title="Stale story", link="https://a.com/stale",
                   published_parsed=_struct_time_days_ago(10)),
        _FakeEntry(title="Undated story", link="https://a.com/undated"),
    ]

    class _FakeResp:
        content = b"<fake rss/>"

    monkeypatch.setattr(scraper._session, "get", lambda *a, **k: _FakeResp())
    monkeypatch.setattr(scraper.feedparser, "parse", lambda content: _FakeFeed(entries))

    result = scraper.scrape_rss("nba", ["https://fake-feed.example/rss"])
    titles = [a["title"] for a in result]

    assert "Fresh story" in titles
    assert "Undated story" in titles  # no date info — kept rather than risk dropping current news
    assert "Stale story" not in titles


def test_scrape_all_builds_custom_feed_for_unknown_interest(monkeypatch):
    called = []

    def fake_scrape_rss(category, urls):
        called.append((category, urls))
        return []

    monkeypatch.setattr(scraper, "scrape_rss", fake_scrape_rss)
    monkeypatch.setattr(scraper, "enrich_article", lambda a, is_google: a)

    scraper.scrape_all(["some totally unknown topic"])
    assert len(called) == 1
    category, urls = called[0]
    assert category == "some totally unknown topic"
    assert "news.google.com" in urls[0]


def test_interest_matching_is_word_level_not_substring():
    # Regression: "ai" is a character-substring of "ukraine"/"sustainability",
    # and "nba" of "sunbathing" — none of these should match.
    assert not scraper._interest_matches_category("ukraine", "ai")
    assert not scraper._interest_matches_category("sustainability", "ai")
    assert not scraper._interest_matches_category("sunbathing", "nba")
    # Word-level containment should still match.
    assert scraper._interest_matches_category("ai", "ai")
    assert scraper._interest_matches_category("nba news", "nba")
    assert scraper._interest_matches_category("news", "world news")
