import anthropic
from database import log_cost, save_summary
from config import ANTHROPIC_API_KEY

# Haiku 4.5 pricing: $1.00 input / $5.00 output per MTok
INPUT_COST_PER_TOKEN = 1.00 / 1_000_000
OUTPUT_COST_PER_TOKEN = 5.00 / 1_000_000

client = anthropic.Anthropic(api_key=ANTHROPIC_API_KEY)

# Cache: url -> summary (avoids re-summarizing same article for multiple users)
_summary_cache = {}

_BRIEFING_SCHEMA = {
    "type": "object",
    "properties": {
        "bullets": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "point": {"type": "string"},
                    "category": {"type": "string"},
                    "keywords": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["point", "category", "keywords"],
                "additionalProperties": False,
            },
        }
    },
    "required": ["bullets"],
    "additionalProperties": False,
}


def summarize_article(title: str, excerpt: str, url: str) -> str:
    if url in _summary_cache:
        return _summary_cache[url]

    if not excerpt:
        return title

    prompt = f"""Summarize this news article in 2 sentences. Be concise and factual.

Title: {title}
Content: {excerpt}

Summary:"""

    try:
        response = client.messages.create(
            model="claude-haiku-4-5",
            max_tokens=150,
            messages=[{"role": "user", "content": prompt}]
        )
        summary = response.content[0].text.strip()

        # Strip markdown headers/prefixes Claude sometimes adds
        for prefix in ["# Summary", "## Summary", "**Summary:**", "Summary:", "#"]:
            if summary.startswith(prefix):
                summary = summary[len(prefix):].strip()

        # The API call happened either way — always record its cost.
        input_tokens = response.usage.input_tokens
        output_tokens = response.usage.output_tokens
        cost = (input_tokens * INPUT_COST_PER_TOKEN) + (output_tokens * OUTPUT_COST_PER_TOKEN)
        log_cost("claude-haiku", "summarize_article", input_tokens + output_tokens, cost)

        # Discard if Claude says it has no content to work with. Persist the
        # empty result as a "tried, nothing usable" marker — otherwise the same
        # doomed article gets re-sent to Claude on every future digest/refresh.
        no_content_phrases = ["don't have access", "cannot access", "no content", "only the title", "beyond the title", "cannot provide", "i cannot", "without access", "no article", "not able to", "i'd be happy to help", "i appreciate your request", "please share the article", "could you please share", "don't see the full article", "i don't see the", "only includes the title", "only the headline", "i'm unable to provide a summary because"]
        if any(p in summary.lower() for p in no_content_phrases):
            summary = ""

        _summary_cache[url] = summary
        save_summary(url, summary)
        return summary

    except Exception as e:
        print(f"  Summarization failed: {e}")
        return excerpt[:200] + "..." if len(excerpt) > 200 else excerpt


def write_morning_briefing(articles: list[dict], user_name: str = None) -> list[dict]:
    """
    Write a bullet-point morning briefing. Returns a list of dicts:
    [{"point": "...", "category": "iran", "keywords": ["iran", "missile"]}]
    Each bullet is clickable and links to articles in that category.
    """
    if not articles:
        return [{"point": "No news found for your interests today.", "category": "", "keywords": []}]

    # Group headlines by category so we can pass all of them
    from collections import defaultdict
    by_cat = defaultdict(list)
    for a in articles:
        by_cat[a["category"]].append(a["title"])

    categories_present = list(by_cat.keys())
    headlines_block = ""
    for cat, titles in by_cat.items():
        for t in titles[:4]:
            headlines_block += f"- [{cat.upper()}] {t}\n"

    prompt = f"""You are writing a morning news briefing as bullet points. Cover EVERY category listed below — each must appear at least once.

Categories to cover: {', '.join(categories_present)}

Each bullet has: a one-sentence factual summary ("point"), the exact lowercase
category name it belongs to ("category"), and 1-3 story keywords ("keywords").

Rules:
- Include at least one bullet per category
- Write 6-10 bullets total
- Be concise and factual
- No emojis anywhere
- Use the exact lowercase category name (e.g. "iran", "ai", "arsenal", "timberwolves", "nba")
- For "timberwolves": only write about the Minnesota Timberwolves specifically — not general NBA news
- For "arsenal": only write about Arsenal FC men's first team — not women's team, not other clubs
- For "nba": write about league-wide NBA news, trades, or stories not specific to one team

Headlines:
{headlines_block}"""

    try:
        # Structured output — guarantees valid JSON, no code-fence stripping
        response = client.messages.create(
            model="claude-haiku-4-5",
            max_tokens=900,
            output_config={"format": {"type": "json_schema", "schema": _BRIEFING_SCHEMA}},
            messages=[{"role": "user", "content": prompt}]
        )
        import json
        bullets = json.loads(response.content[0].text)["bullets"]

        # Strip any emojis Claude may have added despite instructions
        import re
        emoji_pattern = re.compile(
            "[\U00010000-\U0010ffff"
            "\U0001F600-\U0001F64F"
            "\U0001F300-\U0001F5FF"
            "\U0001F680-\U0001F6FF"
            "\U0001F1E0-\U0001F1FF"
            "\u2600-\u26FF\u2700-\u27BF]+",
            flags=re.UNICODE
        )
        for b in bullets:
            if isinstance(b.get("point"), str):
                b["point"] = emoji_pattern.sub("", b["point"]).strip()

        input_tokens = response.usage.input_tokens
        output_tokens = response.usage.output_tokens
        cost = (input_tokens * INPUT_COST_PER_TOKEN) + (output_tokens * OUTPUT_COST_PER_TOKEN)
        log_cost("claude-haiku", "morning_briefing", input_tokens + output_tokens, cost)

        return bullets

    except Exception as e:
        print(f"  Morning briefing failed: {e}")
        # Fallback: one bullet per category
        seen = set()
        bullets = []
        for a in articles[:5]:
            if a["category"] not in seen:
                seen.add(a["category"])
                bullets.append({"point": a["title"], "category": a["category"], "keywords": []})
        return bullets


def summarize_batch(articles: list[dict]) -> list[dict]:
    """Summarize a list of articles, skip ones already cached in memory or DB.
    An empty-string summary means "tried before, article had no usable content"
    — treated as done, so it isn't re-sent to Claude every digest."""
    for article in articles:
        if article.get("summary") is not None:
            # Already in DB — cache it in memory too
            _summary_cache[article["url"]] = article["summary"]
        else:
            print(f"  Summarizing: {article['title'][:60]}...")
            article["summary"] = summarize_article(
                article["title"],
                article.get("excerpt", ""),
                article["url"]
            )
    return articles
