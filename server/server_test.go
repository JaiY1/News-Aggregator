package server

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"news-aggregator/config"
	"news-aggregator/db"
	"news-aggregator/scraper"
	"news-aggregator/summarizer"
)

// TestSignupInterestChips is a structural check on the signup page's preset
// interest checkboxes: right categories, right visible labels (catches the
// nba/ai acronym-casing bug), and the niche categories dropped in favor of
// the custom-interest field stay dropped. It can't catch CSS/layout bugs
// (e.g. the interest-chip margin/height bug from the same change) — that
// needs a real browser layout engine, which this test doesn't have.
func TestSignupInterestChips(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	if err := database.Init(); err != nil {
		t.Fatalf("init db: %v", err)
	}

	srv, err := New(&config.Config{}, database, scraper.New(database), summarizer.New(database, ""))
	if err != nil {
		t.Fatalf("build server: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/signup", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /signup: status %d", rec.Code)
	}

	doc, err := goquery.NewDocumentFromReader(rec.Body)
	if err != nil {
		t.Fatalf("parse signup HTML: %v", err)
	}

	want := map[string]string{
		"nba":        "NBA",
		"ai":         "AI",
		"tech":       "tech",
		"world news": "world news",
		"finance":    "finance",
		"politics":   "politics",
	}

	got := map[string]string{}
	doc.Find(".interest-chip").Each(func(_ int, s *goquery.Selection) {
		value, _ := s.Find("input[name=interests]").Attr("value")
		got[value] = strings.TrimSpace(s.Text())
	})

	if len(got) != len(want) {
		t.Errorf("got %d interest chips, want %d: %v", len(got), len(want), got)
	}
	for value, label := range want {
		gotLabel, ok := got[value]
		if !ok {
			t.Errorf("missing interest chip for %q", value)
			continue
		}
		if gotLabel != label {
			t.Errorf("chip %q label = %q, want %q", value, gotLabel, label)
		}
	}

	// Still fully usable via "add your own" (RSSFeeds/CategoryNegativeKeywords
	// keep their curated feeds and filtering) — just not preset checkboxes.
	for _, dropped := range []string{"timberwolves", "arsenal", "iran", "middle east"} {
		if _, ok := got[dropped]; ok {
			t.Errorf("preset checkbox list should not include %q", dropped)
		}
	}
}
