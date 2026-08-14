// Package config holds application configuration: environment-driven settings
// and the static RSS/Google News feed catalog. Ported from config.py.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// MaxArticleAgeDays drops feed entries older than this. Google News search
// results aren't strictly date-sorted, so old articles matching a query can
// surface alongside genuinely fresh ones.
const MaxArticleAgeDays = 2

// Google News RSS search base URLs. Append a URL-encoded query.
const (
	GN   = "https://news.google.com/rss/search?hl=en-US&gl=US&ceid=US:en&q="
	GNGB = "https://news.google.com/rss/search?hl=en-GB&gl=GB&ceid=GB:en&q="
)

// Config holds runtime configuration resolved from the environment.
type Config struct {
	AnthropicAPIKey string
	AllowedOrigins  []string
	DataDir         string
	AccessCode      string
	Port            string
}

// Load resolves configuration from environment variables, mirroring the
// defaults in config.py. DataDir defaults to the current working directory so
// local dev is unchanged; set DATA_DIR=/data (a mounted volume) in production.
func Load() *Config {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	abs, err := filepath.Abs(dataDir)
	if err == nil {
		dataDir = abs
	}
	// Best-effort create; the DB open will surface a real error if it fails.
	_ = os.MkdirAll(dataDir, 0o755)

	origins := "*"
	if v := os.Getenv("ALLOWED_ORIGINS"); v != "" {
		origins = v
	}
	allowed := make([]string, 0)
	for _, o := range strings.Split(origins, ",") {
		if s := strings.TrimSpace(o); s != "" {
			allowed = append(allowed, s)
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "5001"
	}

	return &Config{
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		AllowedOrigins:  allowed,
		DataDir:         dataDir,
		AccessCode:      os.Getenv("ACCESS_CODE"),
		Port:            port,
	}
}

// DBPath returns the SQLite database path inside DataDir.
func (c *Config) DBPath() string {
	return filepath.Join(c.DataDir, "newsletter.db")
}

// RSSFeeds maps a category to its Google News search feed URLs. Insertion
// order is not significant; matching is by category name.
var RSSFeeds = map[string][]string{
	"timberwolves": {
		GN + "Minnesota+Timberwolves",
		GN + "Anthony+Edwards+NBA",
		GN + "Rudy+Gobert+Timberwolves",
	},
	"arsenal": {
		GNGB + "Arsenal+FC+men",
		GNGB + "Mikel+Arteta+Arsenal",
		GNGB + "Arsenal+Premier+League",
	},
	"iran": {
		GN + "Iran+war+2026",
		GN + "Iran+nuclear+deal",
		GN + "Iran+US+conflict",
	},
	"middle east": {
		GN + "Middle+East+conflict+2026",
		GN + "Israel+Gaza+war",
		GN + "Lebanon+Houthi+2026",
	},
	"nba": {
		GN + "NBA+basketball+-NCAA+-college+-March+Madness",
		GN + "NBA+trades+rumors+-college",
		GN + "NBA+playoffs+standings+2026",
	},
	"ai": {
		GN + "artificial+intelligence+2026",
		GN + "OpenAI+Google+Anthropic",
		GN + "large+language+model",
	},
	"tech": {
		GN + "technology+news+2026",
		GN + "Silicon+Valley+startup",
		GN + "Apple+Microsoft+Google+tech",
	},
	"world news": {
		GN + "world+news+today",
		GN + "international+news+2026",
		GN + "global+breaking+news",
	},
	"finance": {
		GN + "stock+market+2026",
		GN + "economy+inflation+2026",
		GN + "Wall+Street+earnings",
	},
	"politics": {
		GN + "US+politics+2026",
		GN + "Congress+White+House+2026",
		GN + "election+policy+2026",
	},
}

// CategoryOrder lists categories in the same order as the Python dict literal,
// used where a stable presentation order matters (e.g. the signup form).
var CategoryOrder = []string{
	"timberwolves", "arsenal", "iran", "middle east", "nba",
	"ai", "tech", "world news", "finance", "politics",
}

// GoogleNewsCategories is the set of categories served via Google News. Every
// predefined category currently uses Google News, so this mirrors RSSFeeds.
var GoogleNewsCategories = func() map[string]bool {
	m := make(map[string]bool, len(RSSFeeds))
	for k := range RSSFeeds {
		m[k] = true
	}
	return m
}()

// CategoryKeywords are positive keyword filters per category. Unused now that
// all categories use Google News, kept for parity with config.py.
var CategoryKeywords = map[string][]string{}

// CategoryNegativeKeywords blocks spam/off-topic entries per category.
var CategoryNegativeKeywords = map[string][]string{
	"timberwolves": {"bet", "odds", "parlay", "sportsbook", "wager", "spread", "picks", "fantasy", "prop bet", "moneyline"},
	"arsenal":      {"women", "women's", "wsl", "wfc", "ladies", "arsenal women", "arsenal wfc"},
	"nba": {
		"big blue madness", "march madness", "ncaa tournament", "college basketball", "kentucky wildcats", "duke blue devils",
		"bet", "sportsbook", "wager", "parlay", "moneyline", "prop bet", "slots", "casino",
		"panini", "trading card",
	},
}
