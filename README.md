# News-Aggregator (Go)

A Go rewrite of the Python/Flask news aggregator. Scrapes Google News feeds per
user interest, decodes Google News redirect links to the real article, enriches
entries with a body excerpt and og:image, summarizes with Claude, and serves a
web feed plus a JSON API.

Single static binary, embedded HTML templates, pure-Go SQLite (no CGo).

## Layout

| Package       | Responsibility                                             |
| ------------- | ---------------------------------------------------------- |
| `config`      | Env-driven settings + the RSS/Google News feed catalog     |
| `db`          | SQLite persistence (users, articles, digests, cost log)    |
| `scraper`     | Feed fetch/parse, Google News URL decode, enrichment       |
| `summarizer`  | Anthropic (Claude Haiku 4.5) summaries + morning briefing  |
| `server`      | HTTP handlers, web onboarding, feed UI (`net/http`)        |
| `main.go`     | `serve` (HTTP) and `scheduler` (daily digest) entrypoints  |

## Run

```sh
cp .env.example .env      # add your ANTHROPIC_API_KEY
export $(grep -v '^#' .env | xargs)   # or use a dotenv loader
go run . serve            # HTTP server on :5001 (or $PORT)
go run . scheduler        # daily digest loop, fires 07:00 local
```

## Endpoints

```
GET  /health                    health check
GET  /                          landing (redirects to /signup)
GET  /signup, POST /signup      web signup; returns a shareable /feed/<token> link
POST /login                     log an existing user back in by name
POST /users                     create user (JSON API)          [access-code gated]
GET  /users/<token>             get user profile
PUT  /users/<token>             update interests/sources/location
DELETE /users/<token>           delete user                     [access-code gated]
GET  /users/<token>/digest      today's digest for a user
POST /digest/run                trigger digest for all users     [access-code gated]
GET  /articles?category=nba     latest articles by category
GET  /costs                     API cost summary                 [access-code gated]
GET  /costs/total               total spend this month           [access-code gated]
GET  /feed/<token>              feed UI
GET  /feed/<token>/data         JSON the UI polls for digest data
POST /feed/<token>/refresh      kick off a background scrape
GET  /feed/<token>/refresh/status
```

## Config (env)

| Var               | Default          | Notes                                          |
| ----------------- | ---------------- | ---------------------------------------------- |
| `ANTHROPIC_API_KEY` | —              | Required for summaries                          |
| `ALLOWED_ORIGINS` | `*`              | Comma-separated CORS origins                    |
| `DATA_DIR`        | `.`              | SQLite DB location; set to `/data` in prod      |
| `ACCESS_CODE`     | unset            | Gates signup + admin/cost endpoints when set    |
| `PORT`            | `5001` / `8080`  | HTTP port (8080 in the container)               |

## Docker

```sh
docker build -t news-aggregator .
docker run -p 8080:8080 -v newsdata:/data -e ANTHROPIC_API_KEY=... news-aggregator
```

## Tests

```sh
go test -short ./...                 # unit tests only
NETWORK_TESTS=1 go test ./scraper/   # + live Google News decode / feed parse
```
