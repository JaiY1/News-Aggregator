// Package summarizer generates per-article summaries and a bullet-point
// morning briefing via the Anthropic API (Claude Haiku 4.5), and logs token
// cost. Ported from summarizer.py.
package summarizer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"news-aggregator/db"
)

// Haiku 4.5 pricing: $1.00 input / $5.00 output per MTok.
const (
	inputCostPerToken  = 1.00 / 1_000_000
	outputCostPerToken = 5.00 / 1_000_000
	model              = anthropic.ModelClaudeHaiku4_5
)

// Bullet is one item of the morning briefing. Each is clickable in the UI and
// links to articles in its category.
type Bullet struct {
	Point    string   `json:"point"`
	Category string   `json:"category"`
	Keywords []string `json:"keywords"`
}

// Summarizer wraps the Anthropic client, the database (for persisting
// summaries and cost), and an in-memory url→summary cache that avoids
// re-summarizing the same article for multiple users.
type Summarizer struct {
	client anthropic.Client
	db     *db.DB
	cache  sync.Map // url(string) -> summary(string)
}

// New returns a Summarizer using the given API key.
func New(database *db.DB, apiKey string) *Summarizer {
	return &Summarizer{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
		db:     database,
	}
}

// briefingSchema is the JSON schema constraining the morning-briefing output,
// mirroring _BRIEFING_SCHEMA in summarizer.py.
var briefingSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"bullets": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"point":    map[string]any{"type": "string"},
					"category": map[string]any{"type": "string"},
					"keywords": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required":             []string{"point", "category", "keywords"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"bullets"},
	"additionalProperties": false,
}

var summaryPrefixes = []string{"# Summary", "## Summary", "**Summary:**", "Summary:", "#"}

var noContentPhrases = []string{
	"don't have access", "cannot access", "no content", "only the title",
	"beyond the title", "cannot provide", "i cannot", "without access",
	"no article", "not able to", "i'd be happy to help", "i appreciate your request",
	"please share the article", "could you please share", "don't see the full article",
	"i don't see the", "only includes the title", "only the headline",
	"i'm unable to provide a summary because",
}

// SummarizeArticle returns a concise 2-sentence summary of an article, caching
// the result in memory and in the DB. An empty result is persisted as a
// "tried, nothing usable" marker so the article isn't re-sent to Claude later.
func (s *Summarizer) SummarizeArticle(title, excerpt, url string) string {
	if v, ok := s.cache.Load(url); ok {
		return v.(string)
	}
	if excerpt == "" {
		return title
	}

	prompt := fmt.Sprintf(`Summarize this news article in 2 sentences. Be concise and factual.

Title: %s
Content: %s

Summary:`, title, excerpt)

	resp, err := s.client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 150,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		log.Printf("  Summarization failed: %v", err)
		if len(excerpt) > 200 {
			return excerpt[:200] + "..."
		}
		return excerpt
	}

	summary := strings.TrimSpace(firstText(resp))

	// Strip markdown headers/prefixes Claude sometimes adds.
	for _, prefix := range summaryPrefixes {
		if strings.HasPrefix(summary, prefix) {
			summary = strings.TrimSpace(summary[len(prefix):])
		}
	}

	s.logCost("summarize_article", resp.Usage.InputTokens, resp.Usage.OutputTokens)

	// Discard if Claude says it has no content to work with; persist the empty
	// result so the same doomed article isn't re-summarized every run.
	lower := strings.ToLower(summary)
	for _, p := range noContentPhrases {
		if strings.Contains(lower, p) {
			summary = ""
			break
		}
	}

	s.cache.Store(url, summary)
	if err := s.db.SaveSummary(url, summary); err != nil {
		log.Printf("  save summary failed: %v", err)
	}
	return summary
}

// WriteMorningBriefing writes a bullet-point morning briefing covering every
// category present. Uses structured output to guarantee valid JSON.
func (s *Summarizer) WriteMorningBriefing(articles []*db.Article, userName string) []Bullet {
	if len(articles) == 0 {
		return []Bullet{{Point: "No news found for your interests today.", Category: "", Keywords: []string{}}}
	}

	// Group headlines by category, preserving first-seen order.
	byCat := map[string][]string{}
	var catOrder []string
	for _, a := range articles {
		if _, seen := byCat[a.Category]; !seen {
			catOrder = append(catOrder, a.Category)
		}
		byCat[a.Category] = append(byCat[a.Category], a.Title)
	}

	var headlines strings.Builder
	for _, cat := range catOrder {
		titles := byCat[cat]
		if len(titles) > 4 {
			titles = titles[:4]
		}
		for _, t := range titles {
			fmt.Fprintf(&headlines, "- [%s] %s\n", strings.ToUpper(cat), t)
		}
	}

	prompt := fmt.Sprintf(`You are writing a morning news briefing as bullet points. Cover EVERY category listed below — each must appear at least once.

Categories to cover: %s

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
%s`, strings.Join(catOrder, ", "), headlines.String())

	resp, err := s.client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 900,
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: briefingSchema},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		log.Printf("  Morning briefing failed: %v", err)
		return fallbackBullets(articles)
	}

	var parsed struct {
		Bullets []Bullet `json:"bullets"`
	}
	if err := json.Unmarshal([]byte(firstText(resp)), &parsed); err != nil {
		log.Printf("  Morning briefing parse failed: %v", err)
		return fallbackBullets(articles)
	}

	// Strip any emojis Claude may have added despite instructions.
	for i := range parsed.Bullets {
		parsed.Bullets[i].Point = strings.TrimSpace(stripEmoji(parsed.Bullets[i].Point))
		if parsed.Bullets[i].Keywords == nil {
			parsed.Bullets[i].Keywords = []string{}
		}
	}

	s.logCost("morning_briefing", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	return parsed.Bullets
}

// fallbackBullets produces one bullet per category from the first few articles.
func fallbackBullets(articles []*db.Article) []Bullet {
	seen := map[string]bool{}
	var bullets []Bullet
	limit := len(articles)
	if limit > 5 {
		limit = 5
	}
	for _, a := range articles[:limit] {
		if !seen[a.Category] {
			seen[a.Category] = true
			bullets = append(bullets, Bullet{Point: a.Title, Category: a.Category, Keywords: []string{}})
		}
	}
	return bullets
}

// SummarizeBatch summarizes a list of articles, skipping ones already summarized
// (cached in the DB). A non-NULL summary — including an empty string — means
// "already handled", so it isn't re-sent to Claude.
func (s *Summarizer) SummarizeBatch(articles []*db.Article) []*db.Article {
	for _, a := range articles {
		if a.Summary.Valid {
			// Already in DB — cache it in memory too.
			s.cache.Store(a.URL, a.Summary.String)
		} else {
			preview := a.Title
			if len(preview) > 60 {
				preview = preview[:60]
			}
			log.Printf("  Summarizing: %s...", preview)
			sum := s.SummarizeArticle(a.Title, a.Excerpt, a.URL)
			a.Summary = db.NullStr(sum)
		}
	}
	return articles
}

func (s *Summarizer) logCost(operation string, inputTokens, outputTokens int64) {
	cost := float64(inputTokens)*inputCostPerToken + float64(outputTokens)*outputCostPerToken
	if err := s.db.LogCost("claude-haiku", operation, float64(inputTokens+outputTokens), cost); err != nil {
		log.Printf("  log cost failed: %v", err)
	}
}

// firstText returns the text of the first text content block, or "".
func firstText(resp *anthropic.Message) string {
	for _, block := range resp.Content {
		if block.Type == "text" {
			return block.Text
		}
	}
	return ""
}

// stripEmoji removes emoji characters, matching the Unicode ranges in the
// Python emoji regex (all astral-plane characters plus the BMP emoji blocks).
func stripEmoji(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x10000 || (r >= 0x2600 && r <= 0x27BF) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
