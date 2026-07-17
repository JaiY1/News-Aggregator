"""
News Aggregator API
-------------------
Central service exposing news, user management, digests, and cost tracking.

Endpoints:
  GET  /health                    - health check
  GET  /                          - landing page (redirects to signup)
  GET  /signup, POST /signup      - web signup (name + interests); returns a shareable /feed/<token> link
  POST /users                     - create user (JSON API)
  GET  /users/<token>             - get user profile
  PUT  /users/<token>             - update interests/sources/subreddits
  GET  /users/<token>/digest      - get today's digest for a user
  POST /digest/run                - manually trigger digest for all users
  GET  /articles?category=nba     - get latest articles by category
  GET  /costs                     - get API cost summary
  GET  /costs/total               - get total spend this month
"""

from flask import Flask, request, jsonify, render_template, redirect, url_for
from flask_cors import CORS
from database import (
    init_db, create_user, get_user, update_user, get_all_users,
    get_articles_for_user, mark_articles_sent, get_cost_summary
)
from scraper import scrape_all, save_articles
from summarizer import summarize_batch, write_morning_briefing
from config import ALLOWED_ORIGINS, RSS_FEEDS, ACCESS_CODE, DATA_DIR
import sqlite3
import os
import threading
from functools import wraps

_refresh_lock = threading.Lock()
_refresh_status = {}  # token -> "running" | "done" | None
_briefing_cache = {}  # (user_id, article_ids_key) -> briefing bullets

app = Flask(__name__)
CORS(app, origins=ALLOWED_ORIGINS)
init_db()

DB_PATH = str(DATA_DIR / "newsletter.db")


def require_access_code(fn):
    """Gate admin/cost endpoints with ACCESS_CODE, passed as an API key via the
    X-Access-Code header or ?code= query param. No-op when ACCESS_CODE is unset."""
    @wraps(fn)
    def wrapper(*args, **kwargs):
        if ACCESS_CODE:
            supplied = request.headers.get("X-Access-Code") or request.args.get("code")
            if supplied != ACCESS_CODE:
                return jsonify({"error": "Unauthorized"}), 401
        return fn(*args, **kwargs)
    return wrapper


# --- Health check ---

@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok"})


# --- Web onboarding (signup) ---
# No phone/SMS for the demo — signup just collects a name and interests and
# hands back a shareable /feed/<token> link. The link itself is the access
# credential; there's no login/lookup step.

@app.route("/", methods=["GET"])
def landing():
    return redirect(url_for("signup_page"))


@app.route("/signup", methods=["GET"])
def signup_page():
    return render_template("signup.html", categories=list(RSS_FEEDS.keys()), require_code=bool(ACCESS_CODE))


@app.route("/signup", methods=["POST"])
def signup_submit():
    name = request.form.get("name", "").strip()
    location = request.form.get("location", "").strip()
    custom_interest = request.form.get("custom_interest", "").strip()
    interests = request.form.getlist("interests") + [
        c.strip() for c in custom_interest.split(",") if c.strip()
    ]

    if ACCESS_CODE and (request.form.get("code") or "").strip() != ACCESS_CODE:
        return render_template(
            "signup.html", categories=list(RSS_FEEDS.keys()), require_code=True,
            error="Invalid access code.", name=name, location=location,
            selected_interests=interests, custom_interest=custom_interest,
        )

    user = create_user(name)
    update_user(user["token"], interests=interests or ["world news"], location=location)
    feed_url = url_for("feed_ui", token=user["token"], _external=True)
    return render_template("link.html", name=name, feed_url=feed_url)


@app.route("/login", methods=["POST"])
def login_submit():
    """Existing users don't have a password — their feed link/token IS the
    credential. This just accepts either the raw token or a pasted full
    /feed/<token> URL and redirects to the right place."""
    raw = (request.form.get("token") or "").strip()
    token = raw.rstrip("/").split("/")[-1] if raw else ""
    user = get_user(token) if token else None
    if not user:
        return render_template(
            "signup.html", categories=list(RSS_FEEDS.keys()), require_code=bool(ACCESS_CODE),
            login_error="We couldn't find an account for that link.", login_token=raw,
        )
    return redirect(url_for("feed_ui", token=token))


# --- User endpoints ---

@app.route("/users", methods=["POST"])
@require_access_code
def create_user_route():
    data = request.json
    name = data.get("name", "").strip()
    user = create_user(name)
    return jsonify(user)


@app.route("/users/<token>", methods=["GET"])
def get_user_route(token):
    user = get_user(token)
    if not user:
        return jsonify({"error": "User not found"}), 404
    return jsonify(user)


@app.route("/users/<token>", methods=["PUT"])
def update_user_route(token):
    user = get_user(token)
    if not user:
        return jsonify({"error": "User not found"}), 404
    data = request.json
    allowed = ["name", "interests", "sources", "subreddits", "location"]
    updates = {k: v for k, v in data.items() if k in allowed}
    update_user(token, **updates)
    return jsonify(get_user(token))


# --- Digest endpoints ---

@app.route("/users/<token>/digest", methods=["GET"])
def get_digest(token):
    user = get_user(token)
    if not user:
        return jsonify({"error": "User not found"}), 404

    interests = user["interests"] or ["world news"]

    # Scrape + save fresh articles
    articles = scrape_all(interests)
    save_articles(articles)

    # Fetch unsent articles
    unsent = get_articles_for_user(user["id"], interests, limit=10)
    if not unsent:
        return jsonify({"briefing": "No new articles today.", "articles": []})

    summarized = summarize_batch(unsent)
    briefing = write_morning_briefing(summarized, user.get("name"))
    mark_articles_sent(user["id"], [a["id"] for a in summarized])

    return jsonify({
        "user": user["name"] or token,
        "briefing": briefing,
        "articles": [
            {
                "title": a["title"],
                "category": a["category"],
                "summary": a.get("summary"),
                "url": a["url"],
                "source": a["source"],
            }
            for a in summarized
        ]
    })


@app.route("/digest/run", methods=["POST"])
@require_access_code
def run_all_digests():
    """Trigger digest for all users — called by scheduler or manually."""
    users = get_all_users()
    if not users:
        return jsonify({"message": "No users found."}), 200

    all_interests = list({i for u in users for i in u["interests"]})
    articles = scrape_all(all_interests)
    save_articles(articles)

    results = []
    for user in users:
        interests = user["interests"] or ["world news"]
        unsent = get_articles_for_user(user["id"], interests, limit=10)
        if unsent:
            summarized = summarize_batch(unsent)
            briefing = write_morning_briefing(summarized, user.get("name"))
            mark_articles_sent(user["id"], [a["id"] for a in summarized])
            results.append({"user": user["name"] or user["token"], "articles_sent": len(summarized)})
        else:
            results.append({"user": user["name"] or user["token"], "articles_sent": 0})

    return jsonify({"results": results})


# --- Articles endpoint ---

@app.route("/articles", methods=["GET"])
def get_articles():
    category = request.args.get("category", "").strip()
    limit = int(request.args.get("limit", 20))

    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    query = "SELECT * FROM articles"
    params = []
    if category:
        query += " WHERE LOWER(category) LIKE ?"
        params.append(f"%{category.lower()}%")
    query += " ORDER BY scraped_at DESC LIMIT ?"
    params.append(limit)
    rows = conn.execute(query, params).fetchall()
    conn.close()
    return jsonify([dict(r) for r in rows])


# --- Cost tracking endpoints ---

@app.route("/costs", methods=["GET"])
@require_access_code
def get_costs():
    return jsonify(get_cost_summary())


@app.route("/costs/total", methods=["GET"])
@require_access_code
def get_total_costs():
    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    rows = conn.execute("""
        SELECT
            service,
            SUM(cost_usd) as total_usd,
            COUNT(*) as api_calls,
            SUM(units) as total_tokens,
            strftime('%Y-%m', logged_at) as month
        FROM api_costs
        WHERE strftime('%Y-%m', logged_at) = strftime('%Y-%m', 'now')
        GROUP BY service
    """).fetchall()
    overall = conn.execute(
        "SELECT SUM(cost_usd) as grand_total FROM api_costs WHERE strftime('%Y-%m', logged_at) = strftime('%Y-%m', 'now')"
    ).fetchone()
    conn.close()
    return jsonify({
        "month": "current",
        "grand_total_usd": round(overall["grand_total"] or 0, 6),
        "breakdown": [dict(r) for r in rows]
    })


# --- UI routes ---

@app.route("/feed/<token>")
def feed_ui(token):
    user = get_user(token)
    if not user:
        return "User not found", 404
    return render_template("feed.html", token=token)


@app.route("/feed/<token>/data")
def feed_data(token):
    """JSON endpoint the UI calls to get digest data."""
    user = get_user(token)
    if not user:
        return jsonify({"error": "User not found"}), 404

    interests = user["interests"] or ["world news"]

    # Fetch top articles per category from DB (scraping happens via /refresh endpoint).
    # Over-fetch beyond the ~10/category actually shown, since articles without an
    # image get filtered out of the card grid below and we don't want sparse-looking
    # categories as a result.
    from database import get_conn as _get_conn
    _conn = _get_conn()
    all_articles = []
    seen_ids = set()
    for interest in interests:
        rows = _conn.execute("""
            SELECT * FROM articles
            WHERE LOWER(category) LIKE ?
            ORDER BY scraped_at DESC
            LIMIT 15
        """, [f"%{interest.lower()}%"]).fetchall()
        for r in rows:
            d = dict(r)
            if d["id"] not in seen_ids:
                seen_ids.add(d["id"])
                all_articles.append(d)
    _conn.close()

    # Use whatever summaries already exist in the DB (populated by the background
    # refresh). Deliberately DON'T call summarize_batch here — a synchronous Claude
    # call per un-summarized article is what caused gunicorn worker-timeout 500s.
    # Missing summaries just fall back to the excerpt/title client-side.
    summarized = all_articles

    # Cache the briefing per article set so page loads don't re-run Claude
    # (viewing the web feed must not mark articles "sent" — that starves the SMS digest)
    ids_key = tuple(sorted(a["id"] for a in all_articles if a.get("id")))
    briefing = _briefing_cache.get((user["id"], ids_key))
    if briefing is None:
        briefing = write_morning_briefing(summarized, user.get("name"))
        if len(_briefing_cache) > 100:
            _briefing_cache.clear()
        _briefing_cache[(user["id"], ids_key)] = briefing

    # Get today's cost
    from database import get_conn
    conn = get_conn()
    cost_row = conn.execute(
        "SELECT SUM(cost_usd) as total FROM api_costs WHERE DATE(logged_at) = DATE('now')"
    ).fetchone()
    conn.close()
    cost_today = (cost_row["total"] or 0.0) if cost_row else 0.0

    return jsonify({
        "user": user["name"] or token,
        "briefing": briefing,
        # Only articles with a real image make it into the card grid — an image-less
        # card looks broken next to ones with photos. The briefing text above still
        # draws on the full article set, so this doesn't shrink news coverage there.
        "articles": [
            {
                "id": a.get("id"),
                "title": a["title"],
                "category": a["category"],
                "summary": a.get("summary"),
                "url": a["url"],
                "source": a.get("source", ""),
                "image_url": a.get("image_url"),
            }
            for a in summarized
            if a.get("image_url")
        ],
        "cost_today": cost_today,
    })


@app.route("/feed/<token>/refresh", methods=["POST"])
def refresh_feed(token):
    """Kick off a background scrape for a user. Returns immediately."""
    user = get_user(token)
    if not user:
        return jsonify({"error": "User not found"}), 404

    if _refresh_status.get(token) == "running":
        return jsonify({"status": "already_running"})

    interests = user["interests"] or ["world news"]

    def _run():
        try:
            articles = scrape_all(interests)
            save_articles(articles)
            # Summarize here, in the background, so the synchronous /data endpoint
            # never has to make slow Claude calls on page load. summarize_batch
            # persists each summary to the DB (via save_summary), so a later /data
            # read picks them up already-summarized. Doing this inline in /data was
            # what blew past gunicorn's worker timeout and 500'd on Render.
            try:
                summarize_batch(articles)
            except Exception as e:
                print(f"Background summarize failed: {e}")
        finally:
            _refresh_status[token] = "done"

    # Set the flag before the thread starts so a double-click can't launch two scrapes
    _refresh_status[token] = "running"
    threading.Thread(target=_run, daemon=True).start()
    return jsonify({"status": "started"})


@app.route("/feed/<token>/refresh/status")
def refresh_status(token):
    return jsonify({"status": _refresh_status.get(token, "idle")})


if __name__ == "__main__":
    app.run(debug=os.environ.get("FLASK_DEBUG") == "1", port=int(os.environ.get("PORT", 5001)))
