"""
Run this once to create a test user and fire the digest locally.
Usage: python3 setup_test_user.py
"""
from database import init_db, create_user, update_user, get_user
from scheduler import run_daily_digest

init_db()

# Create a test user
phone = "test_user"
create_user(phone, name="Jai")
update_user(phone,
    interests=["timberwolves", "arsenal", "ai", "iran", "middle east"],
    subreddits=["timberwolves", "gunners", "artificial", "worldnews"],
)

user = get_user(phone)
print(f"Test user created: {user['name']} | Interests: {user['interests']}")
print("\nRunning digest now...\n")

run_daily_digest()
