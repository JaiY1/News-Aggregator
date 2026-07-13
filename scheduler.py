import schedule
import time
from database import init_db, get_all_users, get_articles_for_user, mark_articles_sent, get_cost_summary
from scraper import scrape_all, save_articles
from summarizer import summarize_batch, write_morning_briefing


def run_digest_for_user(user: dict):
    print(f"\n--- Processing digest for {user.get('name') or user['phone_number']} ---")

    interests = user["interests"] or ["world news"]
    print(f"  Interests: {interests}")

    # Articles are scraped once for all users in run_daily_digest —
    # here we only pull unsent ones from the DB

    # Fetch unsent articles from DB matching their interests
    unsent = get_articles_for_user(user["id"], interests, limit=10)
    if not unsent:
        print("  No new articles to send.")
        return

    # Summarize
    summarized = summarize_batch(unsent)

    # Write morning briefing
    briefing = write_morning_briefing(summarized, user.get("name"))

    # Print digest (later this will be sent via SMS)
    print("\n" + "="*60)
    print(f"MORNING DIGEST — {user.get('name') or user['phone_number']}")
    print("="*60)
    print()
    for b in briefing:
        print(f"  • [{b.get('category', '')}] {b.get('point', '')}")
    print()
    print("--- TOP STORIES ---")
    for a in summarized[:5]:
        print(f"\n[{a['category'].upper()}] {a['title']}")
        print(f"  {a.get('summary') or a.get('excerpt', '')[:150]}")
        print(f"  {a['url']}")
    print("="*60)

    # Mark as sent
    mark_articles_sent(user["id"], [a["id"] for a in summarized])


def run_daily_digest():
    print("\n🗞  Running daily digest...")
    users = get_all_users()
    if not users:
        print("  No users found. Add a user to get started.")
        return

    # Dedupe scraping — collect all unique interests across users
    all_interests = list({i for u in users for i in (u["interests"] or ["world news"])})
    if all_interests:
        print(f"  Scraping for interests: {all_interests}")
        articles = scrape_all(all_interests)
        save_articles(articles)

    for user in users:
        run_digest_for_user(user)

    # Print cost summary
    print("\n💰 Cost Summary:")
    for row in get_cost_summary():
        print(f"  {row['date']} | {row['service']}: ${row['total']:.6f}")


def start_scheduler(run_now: bool = False):
    init_db()

    if run_now:
        run_daily_digest()

    # Schedule daily at 7:00 AM
    schedule.every().day.at("07:00").do(run_daily_digest)
    print("\n⏰ Scheduler running — digest will fire at 7:00 AM daily.")
    print("   Press Ctrl+C to stop.\n")

    while True:
        schedule.run_pending()
        time.sleep(60)


if __name__ == "__main__":
    # Run immediately for local testing
    start_scheduler(run_now=True)
