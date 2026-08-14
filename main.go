// Command news-aggregator runs the news aggregator. With no arguments (or
// "serve") it starts the HTTP server. With "scheduler" it runs the daily digest
// loop, firing at 07:00 local time. This unifies the Python api.py (web) and
// scheduler.py (cron) entrypoints into one binary.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"news-aggregator/config"
	"news-aggregator/db"
	"news-aggregator/scraper"
	"news-aggregator/server"
	"news-aggregator/summarizer"
)

func main() {
	cfg := config.Load()

	database, err := db.Open(cfg.DBPath())
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer database.Close()
	if err := database.Init(); err != nil {
		log.Fatalf("init db: %v", err)
	}
	log.Println("DB initialized.")

	scr := scraper.New(database)
	sum := summarizer.New(database, cfg.AnthropicAPIKey)

	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "scheduler":
		runScheduler(database, scr, sum, true)
	case "serve":
		serve(cfg, database, scr, sum)
	default:
		log.Fatalf("unknown mode %q (use 'serve' or 'scheduler')", mode)
	}
}

func serve(cfg *config.Config, database *db.DB, scr *scraper.Scraper, sum *summarizer.Summarizer) {
	srv, err := server.New(cfg, database, scr, sum)
	if err != nil {
		log.Fatalf("build server: %v", err)
	}
	addr := "0.0.0.0:" + cfg.Port
	log.Printf("Listening on %s", addr)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// runScheduler runs the daily digest at 07:00 local time, optionally firing
// once immediately on startup for local testing.
func runScheduler(database *db.DB, scr *scraper.Scraper, sum *summarizer.Summarizer, runNow bool) {
	if runNow {
		runDailyDigest(database, scr, sum)
	}
	log.Println("Scheduler running — digest will fire at 07:00 daily.")
	for {
		time.Sleep(untilNext(7, 0))
		runDailyDigest(database, scr, sum)
	}
}

// untilNext returns the duration until the next occurrence of hour:min local.
func untilNext(hour, min int) time.Duration {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return time.Until(next)
}

// runDailyDigest scrapes once for all users' interests, then summarizes and
// marks each user's unsent articles. Ported from scheduler.py.
func runDailyDigest(database *db.DB, scr *scraper.Scraper, sum *summarizer.Summarizer) {
	log.Println("Running daily digest...")
	users, err := database.GetAllUsers()
	if err != nil {
		log.Printf("  get users: %v", err)
		return
	}
	if len(users) == 0 {
		log.Println("  No users found. Add a user to get started.")
		return
	}

	// Dedupe scraping across all users' interests.
	interestSet := map[string]bool{}
	for _, u := range users {
		for _, i := range interestsOrDefault(u.Interests) {
			interestSet[i] = true
		}
	}
	allInterests := make([]string, 0, len(interestSet))
	for i := range interestSet {
		allInterests = append(allInterests, i)
	}
	if len(allInterests) > 0 {
		log.Printf("  Scraping for interests: %v", allInterests)
		articles := scr.ScrapeAll(allInterests)
		scr.SaveArticles(articles)
	}

	for _, user := range users {
		name := user.Name
		if name == "" {
			name = user.PhoneNumber
		}
		log.Printf("--- Processing digest for %s ---", name)
		interests := interestsOrDefault(user.Interests)
		unsent, _ := database.GetArticlesForUser(user.ID, interests, 10)
		if len(unsent) == 0 {
			log.Println("  No new articles to send.")
			continue
		}
		summarized := sum.SummarizeBatch(unsent)
		briefing := sum.WriteMorningBriefing(summarized, user.Name)
		for _, b := range briefing {
			log.Printf("  • [%s] %s", b.Category, b.Point)
		}
		ids := make([]int64, 0, len(summarized))
		for _, a := range summarized {
			ids = append(ids, a.ID)
		}
		if err := database.MarkArticlesSent(user.ID, ids); err != nil {
			log.Printf("  mark sent: %v", err)
		}
	}

	log.Println("Cost Summary:")
	rows, _ := database.GetCostSummary()
	for _, row := range rows {
		log.Printf("  %s | %s: $%.6f", row.Date, row.Service, row.Total)
	}
}

func interestsOrDefault(in []string) []string {
	if len(in) == 0 {
		return []string{"world news"}
	}
	return in
}
