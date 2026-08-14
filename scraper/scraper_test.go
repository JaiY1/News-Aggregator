package scraper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"news-aggregator/config"
	"news-aggregator/db"
)

func TestParseBatchExecute(t *testing.T) {
	// Shape of a real batchexecute response: XSSI guard, blank line, then a
	// JSON array whose wrb.fr row carries ["garturlres","<url>"] at index 2.
	body := ")]}'\n\n" +
		`[["wrb.fr","Fbv4je","[\"garturlres\",\"https://www.espn.com/nba/story/_/id/1\"]",null,null,null,"generic"],["di",44],["af.httprm",44,"x",8]]`
	got, ok := parseBatchExecute(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if want := "https://www.espn.com/nba/story/_/id/1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseBatchExecuteGarbage(t *testing.T) {
	if _, ok := parseBatchExecute("not a batch response"); ok {
		t.Fatal("expected failure on garbage input")
	}
}

func TestInterestMatchesCategory(t *testing.T) {
	cases := []struct {
		interest, cat string
		want          bool
	}{
		{"nba", "nba", true},
		{"middle east", "middle east", true},
		{"ai", "ukraine", false}, // must NOT match on shared letters
		{"timberwolves", "nba", false},
	}
	for _, c := range cases {
		if got := interestMatchesCategory(c.interest, c.cat); got != c.want {
			t.Errorf("interestMatchesCategory(%q,%q)=%v want %v", c.interest, c.cat, got, c.want)
		}
	}
}

func TestNearDuplicateTitle(t *testing.T) {
	seen := []map[string]bool{titleWords("Lakers beat Celtics in overtime thriller")}
	if !isNearDuplicateTitle("Lakers defeat Celtics in overtime thriller", seen) {
		t.Error("expected reworded headline to be a near-duplicate")
	}
	if isNearDuplicateTitle("Warriors trade for new center", seen) {
		t.Error("unrelated headline should not be a duplicate")
	}
}

// newTestScraper opens a temp DB and returns a Scraper for live tests.
func newTestScraper(t *testing.T) *Scraper {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := database.Init(); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database)
}

// TestResolveURLLive hits Google News to decode a real RSS article URL. Skipped
// with -short or when NETWORK_TESTS is unset.
func TestResolveURLLive(t *testing.T) {
	if testing.Short() || os.Getenv("NETWORK_TESTS") == "" {
		t.Skip("set NETWORK_TESTS=1 to run live Google News decode test")
	}
	s := newTestScraper(t)
	// Grab a fresh Google News article URL from a live feed.
	articles := s.scrapeRSS("nba", []string{config.GN + "NBA+basketball"})
	if len(articles) == 0 {
		t.Fatal("no articles scraped from feed")
	}
	var gnURL string
	for _, a := range articles {
		if strings.Contains(a.URL, "news.google.com") {
			gnURL = a.URL
			break
		}
	}
	if gnURL == "" {
		t.Skip("feed returned no google news redirect URLs")
	}
	decoded := s.resolveURL(gnURL)
	if strings.Contains(decoded, "news.google.com") || decoded == gnURL {
		t.Fatalf("resolveURL did not decode: %q", decoded)
	}
	t.Logf("decoded %s -> %s", gnURL[:40]+"...", decoded)
}

// TestScrapeRSSLive validates feed fetching + parsing (title suffix stripping,
// source extraction) without the slow enrichment path.
func TestScrapeRSSLive(t *testing.T) {
	if testing.Short() || os.Getenv("NETWORK_TESTS") == "" {
		t.Skip("set NETWORK_TESTS=1 to run live feed parse test")
	}
	s := newTestScraper(t)
	articles := s.scrapeRSS("nba", []string{config.GN + "NBA+basketball"})
	if len(articles) == 0 {
		t.Fatal("no articles parsed")
	}
	for _, a := range articles {
		if a.Title == "" || a.URL == "" {
			t.Errorf("article missing title/url: %+v", a)
		}
		// Google News appends " - Publisher"; it should have been stripped.
		if a.Source != "" && strings.HasSuffix(a.Title, " - "+a.Source) {
			t.Errorf("publisher suffix not stripped: %q", a.Title)
		}
	}
	t.Logf("parsed %d articles; first: %q (source=%q)", len(articles), articles[0].Title, articles[0].Source)
}
