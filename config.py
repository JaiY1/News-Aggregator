import os
from pathlib import Path
from dotenv import load_dotenv

load_dotenv(os.path.join(os.path.dirname(__file__), ".env"))

ANTHROPIC_API_KEY = os.getenv("ANTHROPIC_API_KEY")

# Comma-separated list of origins allowed to call this API cross-origin (e.g. the
# website frontend). Defaults to "*" for local development.
ALLOWED_ORIGINS = [o.strip() for o in os.getenv("ALLOWED_ORIGINS", "*").split(",")]

# Where the SQLite DB lives. Defaults to the project root so local dev is
# unchanged; set DATA_DIR=/data (a mounted volume) in production so redeploys
# don't wipe users/articles.
DATA_DIR = Path(os.getenv("DATA_DIR") or Path(__file__).resolve().parent).resolve()
DATA_DIR.mkdir(parents=True, exist_ok=True)

# Shared access gate. When set, signup requires this code, and it also acts as
# an API key (via X-Access-Code header or ?code=) for the admin/cost endpoints.
# Unset locally = no gate.
ACCESS_CODE = os.getenv("ACCESS_CODE")

# Google News search results aren't strictly date-sorted, so old articles matching
# a query can surface alongside genuinely fresh ones — drop anything older than this.
MAX_ARTICLE_AGE_DAYS = 2

GN = "https://news.google.com/rss/search?hl=en-US&gl=US&ceid=US:en&q="
GN_GB = "https://news.google.com/rss/search?hl=en-GB&gl=GB&ceid=GB:en&q="

RSS_FEEDS = {
    "timberwolves": [
        GN + "Minnesota+Timberwolves",
        GN + "Anthony+Edwards+NBA",
        GN + "Rudy+Gobert+Timberwolves",
    ],
    "arsenal": [
        GN_GB + "Arsenal+FC+men",
        GN_GB + "Mikel+Arteta+Arsenal",
        GN_GB + "Arsenal+Premier+League",
    ],
    "iran": [
        GN + "Iran+war+2026",
        GN + "Iran+nuclear+deal",
        GN + "Iran+US+conflict",
    ],
    "middle east": [
        GN + "Middle+East+conflict+2026",
        GN + "Israel+Gaza+war",
        GN + "Lebanon+Houthi+2026",
    ],
    "nba": [
        GN + "NBA+basketball+-NCAA+-college+-March+Madness",
        GN + "NBA+trades+rumors+-college",
        GN + "NBA+playoffs+standings+2026",
    ],
    "ai": [
        GN + "artificial+intelligence+2026",
        GN + "OpenAI+Google+Anthropic",
        GN + "large+language+model",
    ],
    "tech": [
        GN + "technology+news+2026",
        GN + "Silicon+Valley+startup",
        GN + "Apple+Microsoft+Google+tech",
    ],
    "world news": [
        GN + "world+news+today",
        GN + "international+news+2026",
        GN + "global+breaking+news",
    ],
    "finance": [
        GN + "stock+market+2026",
        GN + "economy+inflation+2026",
        GN + "Wall+Street+earnings",
    ],
    "politics": [
        GN + "US+politics+2026",
        GN + "Congress+White+House+2026",
        GN + "election+policy+2026",
    ],
}

# All categories now use Google News — no keyword filtering needed
GOOGLE_NEWS_CATEGORIES = set(RSS_FEEDS.keys())

CATEGORY_KEYWORDS = {}

# Negative filters to block spam
CATEGORY_NEGATIVE_KEYWORDS = {
    "timberwolves": ["bet", "odds", "parlay", "sportsbook", "wager", "spread", "picks", "fantasy", "prop bet", "moneyline"],
    "arsenal": ["women", "women's", "wsl", "wfc", "ladies", "arsenal women", "arsenal wfc"],
    "nba": [
        "big blue madness", "march madness", "ncaa tournament", "college basketball", "kentucky wildcats", "duke blue devils",
        # Gambling/ad spam that slips through Google News (unambiguous terms only —
        # avoid "picks"/"fantasy"/"spread", which also show up in legit draft/trade coverage)
        "bet", "sportsbook", "wager", "parlay", "moneyline", "prop bet", "slots", "casino",
        "panini", "trading card",
    ],
}
