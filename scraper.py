import re
import calendar
import feedparser
import requests
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from bs4 import BeautifulSoup
from database import save_article, get_articles_by_source_urls
from config import (
    RSS_FEEDS, CATEGORY_KEYWORDS, CATEGORY_NEGATIVE_KEYWORDS, GOOGLE_NEWS_CATEGORIES,
    MAX_ARTICLE_AGE_DAYS, GN,
)
from datetime import datetime, timedelta, timezone
from concurrent.futures import ThreadPoolExecutor, as_completed
from urllib.parse import urlparse, quote_plus
from googlenewsdecoder import new_decoderv1
import time

# Shared session with automatic retry/backoff for transient network errors
# (connection resets, timeouts, and 429/5xx) so a single flaky host doesn't
# fail scraping outright.
_session = requests.Session()
_retry = Retry(
    total=3,
    backoff_factor=0.5,
    status_forcelist=[429, 500, 502, 503, 504],
    allowed_methods=["GET"],
)
_session.mount("https://", HTTPAdapter(max_retries=_retry))
_session.mount("http://", HTTPAdapter(max_retries=_retry))

PAYWALLED_DOMAINS = {
    "nytimes.com", "wsj.com", "washingtonpost.com", "ft.com",
    "theatlantic.com", "thetimes.co.uk", "telegraph.co.uk",
    "economist.com", "newyorker.com", "bloomberg.com",
    "hbr.org", "businessinsider.com", "insider.com",
    "latimes.com", "bostonglobe.com", "sfchronicle.com",
}

HEADERS = {"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/120 Safari/537.36"}


def resolve_url(url: str) -> str:
    """Decode Google News URL to the real article URL."""
    if "news.google.com" not in url:
        return url
    try:
        result = new_decoderv1(url, interval=2)
        if result.get("status") and result.get("decoded_url"):
            return result["decoded_url"]
    except Exception:
        pass
    return url


def domain_to_name(url: str) -> str:
    """Convert a URL to a clean publisher name, e.g. 'startribune.com' → 'Star Tribune'."""
    try:
        domain = urlparse(url).netloc.lower().removeprefix("www.")
        # Strip common TLDs/subdomains for display
        name = domain.split(".")[0]
        return name.replace("-", " ").title()
    except Exception:
        return url


def fetch_og_image(url: str):
    """Fetch og:image from an article page.

    Some sites (e.g. WordPress-based ones like NBC New York) pack the <head>
    with tracking/analytics tags before the og:image meta tag, pushing it well
    past a small byte budget — read until </head> (with a generous safety
    cap) instead of cutting off early, or the meta tag gets missed entirely.
    """
    try:
        resp = _session.get(url, timeout=8, headers=HEADERS, stream=True)
        content = b""
        for chunk in resp.iter_content(8192):
            content += chunk
            if b"</head>" in content or b"og:image" in content or len(content) > 500_000:
                break
        soup = BeautifulSoup(content, "html.parser")
        tag = soup.find("meta", property="og:image") or soup.find("meta", attrs={"name": "og:image"})
        if tag and tag.get("content"):
            return tag["content"]
    except Exception:
        pass
    return None


def is_paywalled(url: str) -> bool:
    domain = urlparse(url).netloc.lower().removeprefix("www.")
    return any(domain == p or domain.endswith("." + p) for p in PAYWALLED_DOMAINS)


def fetch_article_body(url: str) -> str:
    """Scrape full article body text when RSS provides no excerpt."""
    try:
        resp = _session.get(url, timeout=6, headers=HEADERS, stream=True)
        content = b""
        for chunk in resp.iter_content(8192):
            content += chunk
            if len(content) > 200000:
                break
        soup = BeautifulSoup(content, "html.parser")

        # Remove noise tags
        for tag in soup(["script", "style", "nav", "header", "footer", "aside", "form", "ads"]):
            tag.decompose()

        # Try common article content containers in order of specificity
        body = (
            soup.find("article") or
            soup.find(attrs={"itemprop": "articleBody"}) or
            soup.find(class_=lambda c: c and any(x in c.lower() for x in ["article-body", "article__body", "story-body", "post-body", "entry-content", "article-content"])) or
            soup.find("main")
        )

        if body:
            paragraphs = body.find_all("p")
        else:
            paragraphs = soup.find_all("p")

        text = " ".join(p.get_text(strip=True) for p in paragraphs if len(p.get_text(strip=True)) > 40)
        return text[:1000] if text else ""
    except Exception:
        return ""


def enrich_article(article: dict, is_google_news: bool) -> dict:
    """Resolve real URL, check paywall, set real source name, fetch og:image and body."""
    url = article["url"]

    if is_google_news and "news.google.com" in url:
        resolved = resolve_url(url)
        if resolved and "news.google.com" not in resolved:
            article["url"] = resolved
            url = resolved
            # Set source name from actual domain if not already set by feedparser
            if not article.get("source") or article["source"] == "Google News":
                article["source"] = domain_to_name(url)

    if is_paywalled(url):
        article["_paywalled"] = True
        return article

    real_url = url and "news.google.com" not in url

    # Fetch full article body if RSS gave us no useful excerpt
    if real_url and len(article.get("excerpt") or "") < 100:
        body = fetch_article_body(url)
        if body:
            article["excerpt"] = body

    if not article.get("image_url") and real_url:
        article["image_url"] = fetch_og_image(url)

    return article


_TITLE_STOPWORDS = {
    "the", "a", "an", "in", "on", "at", "to", "of", "and", "for", "is", "are",
    "after", "with", "who", "his", "her", "their", "was", "has", "have",
}


def _title_words(title: str) -> set:
    return {
        w for w in re.findall(r"[a-z0-9]+", title.lower())
        if w not in _TITLE_STOPWORDS and len(w) > 2
    }


def _is_near_duplicate_title(title: str, seen_word_sets: list) -> bool:
    """Catch different publishers covering the same story with reworded
    headlines (word-overlap ratio) — not exhaustive, just a cheap first pass.
    """
    words = _title_words(title)
    if not words:
        return False
    for other in seen_word_sets:
        overlap = len(words & other) / len(words | other)
        if overlap >= 0.6:
            return True
    return False


def _parse_published(entry) -> datetime:
    """Return a timezone-aware UTC datetime from the entry's published date, or
    None if feedparser couldn't parse one."""
    parsed = entry.get("published_parsed")
    if not parsed:
        return None
    try:
        return datetime.fromtimestamp(calendar.timegm(parsed), tz=timezone.utc)
    except Exception:
        return None


def scrape_rss(category: str, urls: list) -> list:
    articles = []
    for feed_url in urls:
        try:
            # Fetch with an explicit timeout first — feedparser.parse(url) has no
            # timeout of its own and can hang indefinitely on a slow feed server.
            resp = _session.get(feed_url, timeout=8, headers=HEADERS)
            feed = feedparser.parse(resp.content)
            for entry in feed.entries[:10]:
                excerpt = ""
                if hasattr(entry, "summary"):
                    soup = BeautifulSoup(entry.summary, "html.parser")
                    excerpt = soup.get_text()[:500]

                published_dt = _parse_published(entry)
                # Google News search results aren't strictly date-sorted, so old
                # articles matching the query show up alongside fresh ones —
                # drop anything we can confirm is too old. Keep entries with no
                # parseable date rather than risk dropping legitimate current ones.
                if published_dt and published_dt < datetime.now(timezone.utc) - timedelta(days=MAX_ARTICLE_AGE_DAYS):
                    continue
                published = published_dt.isoformat() if published_dt else entry.get("published")

                image_url = None
                if hasattr(entry, "media_thumbnail") and entry.media_thumbnail:
                    image_url = entry.media_thumbnail[0].get("url")
                elif hasattr(entry, "media_content") and entry.media_content:
                    image_url = entry.media_content[0].get("url")
                elif hasattr(entry, "enclosures") and entry.enclosures:
                    for enc in entry.enclosures:
                        if enc.get("type", "").startswith("image"):
                            image_url = enc.get("url")
                            break

                # For Google News entries, prefer entry.source.value (real publisher name)
                source = ""
                if hasattr(entry, "source") and isinstance(entry.source, dict):
                    source = entry.source.get("value", "") or entry.source.get("title", "")
                if not source:
                    source = feed.feed.get("title", feed_url)

                # Strip " - Publisher Name" suffix Google News appends to titles
                title = entry.get("title", "")
                if source and title.endswith(f" - {source}"):
                    title = title[: -len(f" - {source}")]

                link = entry.get("link", "")
                articles.append({
                    "url": link,
                    "source_url": link,  # raw feed link, before Google News redirect resolution
                    "title": title,
                    "source": source,
                    "category": category,
                    "excerpt": excerpt,
                    "summary": None,
                    "published_at": published,
                    "image_url": image_url,
                })
            time.sleep(0.2)
        except Exception as e:
            print(f"  Failed to scrape {feed_url}: {e}")
    return articles


def _interest_matches_category(interest_lower: str, cat: str) -> bool:
    """Word-level match between a user interest and a feed category. The old
    character-substring test ("ai" in "ukraine") matched unrelated categories
    on shared letters — feeding a "ukraine" user AI articles while ALSO
    skipping the on-the-fly Google News feed for their actual topic (the
    fallback only fires when nothing matched)."""
    if interest_lower == cat:
        return True
    iw = set(re.findall(r"[a-z0-9]+", interest_lower))
    cw = set(re.findall(r"[a-z0-9]+", cat))
    return bool(iw) and bool(cw) and (iw <= cw or cw <= iw)


def scrape_all(interests: list) -> list:
    """Scrape feeds for a list of interest categories."""
    all_articles = []
    seen_urls = set()

    for interest in interests:
        interest_lower = interest.lower()
        matched = [
            (cat, urls) for cat, urls in RSS_FEEDS.items()
            if _interest_matches_category(interest_lower, cat)
        ]
        if not matched:
            # Not one of the predefined categories — build a Google News search
            # feed for it on the fly instead of silently falling back to
            # "world news" (which just re-shows articles the user already has
            # and drops their actual interest).
            matched = [(interest_lower, [GN + quote_plus(interest.strip())])]

        for cat, urls in matched:
            is_google = cat in GOOGLE_NEWS_CATEGORIES or any("news.google.com" in u for u in urls)
            print(f"  Scraping {cat} ({'Google News' if is_google else 'RSS'})...")
            articles = scrape_rss(cat, urls)

            negative_keywords = CATEGORY_NEGATIVE_KEYWORDS.get(cat, [])
            keywords = CATEGORY_KEYWORDS.get(cat, [])
            seen_sources_in_cat = {}
            seen_titles_in_cat = []

            candidates = []
            for a in articles:
                if not a["url"] or a["url"] in seen_urls:
                    continue
                text = (a["title"] + " " + a.get("excerpt", "")).lower()
                if not is_google and keywords:
                    if not any(kw.lower() in text for kw in keywords):
                        continue
                if negative_keywords:
                    if any(nkw.lower() in text for nkw in negative_keywords):
                        continue
                source = a.get("source", "")
                if seen_sources_in_cat.get(source, 0) >= 2:
                    continue
                if _is_near_duplicate_title(a["title"], seen_titles_in_cat):
                    continue
                seen_sources_in_cat[source] = seen_sources_in_cat.get(source, 0) + 1
                seen_titles_in_cat.append(_title_words(a["title"]))
                candidates.append(a)

            # Skip re-enriching (re-decoding Google News redirects, re-fetching
            # body/og:image) for candidates we've already scraped in a previous
            # run — reuse the cached row instead. This is what makes repeat
            # refreshes fast instead of re-doing full network work every time.
            known = get_articles_by_source_urls([c["source_url"] for c in candidates])
            to_enrich = []
            for c in candidates:
                cached = known.get(c["source_url"])
                if cached and cached.get("url") and cached["url"] not in seen_urls:
                    seen_urls.add(cached["url"])
                    all_articles.append({
                        "url": cached["url"],
                        "source_url": c["source_url"],
                        "title": cached["title"],
                        "source": cached["source"],
                        "category": cat,
                        "excerpt": cached["excerpt"],
                        "summary": cached["summary"],
                        "image_url": cached["image_url"],
                        "published_at": cached["published_at"],
                    })
                elif not cached:
                    to_enrich.append(c)

            print(f"  Enriching {len(to_enrich)} new articles ({len(candidates) - len(to_enrich)} already cached)...")
            with ThreadPoolExecutor(max_workers=3) as executor:
                futures = {executor.submit(enrich_article, a, is_google): a for a in to_enrich}
                for future in as_completed(futures):
                    try:
                        enriched = future.result()
                        if enriched.get("_paywalled"):
                            continue
                        if enriched["url"] and enriched["url"] not in seen_urls:
                            seen_urls.add(enriched["url"])
                            all_articles.append(enriched)
                    except Exception:
                        pass

    return all_articles


def save_articles(articles: list) -> list:
    ids = []
    for article in articles:
        article_id = save_article(article)
        if article_id:
            ids.append(article_id)
    print(f"  Saved {len(ids)} new articles.")
    return ids
