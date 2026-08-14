// Package scraper fetches Google News / RSS feeds, decodes Google News
// redirect URLs to the real article, enriches entries with a body excerpt and
// og:image, and dedupes across sources. Ported from scraper.py.
package scraper

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed/rss"

	"news-aggregator/config"
	"news-aggregator/db"
)

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/120 Safari/537.36"

var paywalledDomains = map[string]bool{
	"nytimes.com": true, "wsj.com": true, "washingtonpost.com": true, "ft.com": true,
	"theatlantic.com": true, "thetimes.co.uk": true, "telegraph.co.uk": true,
	"economist.com": true, "newyorker.com": true, "bloomberg.com": true,
	"hbr.org": true, "businessinsider.com": true, "insider.com": true,
	"latimes.com": true, "bostonglobe.com": true, "sfchronicle.com": true,
}

// Scraper holds the shared HTTP client and DB handle used across a scrape run.
type Scraper struct {
	db     *db.DB
	client *http.Client
}

// New returns a Scraper backed by the given database.
func New(database *db.DB) *Scraper {
	return &Scraper{
		db: database,
		// No global timeout — each request sets its own via context, matching
		// the per-call timeouts in the Python version (8s feeds, 6s bodies).
		client: &http.Client{},
	}
}

// get performs a GET with the browser User-Agent, a per-request timeout, and
// retry/backoff on transient failures (connection errors and 429/5xx), so a
// single flaky host doesn't fail the whole scrape.
func (s *Scraper) get(rawURL string, timeout time.Duration) (*http.Response, error) {
	var lastErr error
	retryStatus := map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			// backoff_factor 0.5 → 0.5, 1.0, 2.0s
			time.Sleep(time.Duration(float64(time.Second) * 0.5 * float64(int(1)<<(attempt-1))))
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		if retryStatus[resp.StatusCode] {
			resp.Body.Close()
			cancel()
			lastErr = &httpStatusError{resp.StatusCode}
			continue
		}
		// Caller reads and closes Body; cancel must outlive the read, so wrap
		// the body to cancel on Close.
		resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	return nil, lastErr
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return http.StatusText(e.code) }

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// domainToName converts a URL to a clean publisher name, e.g.
// "startribune.com" → "Star Tribune".
func domainToName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	name := strings.SplitN(host, ".", 2)[0]
	name = strings.ReplaceAll(name, "-", " ")
	return titleCase(name)
}

// titleCase upper-cases the first letter of each word, like Python str.title().
func titleCase(s string) string {
	return strings.Join(mapWords(strings.Fields(s), func(w string) string {
		if w == "" {
			return w
		}
		return strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}), " ")
}

func mapWords(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, w := range in {
		out[i] = f(w)
	}
	return out
}

func isPaywalled(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	for p := range paywalledDomains {
		if host == p || strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

// fetchOGImage fetches the og:image from an article page. Some sites pack the
// <head> with tracking tags before og:image, so read until </head> (with a
// generous cap) rather than cutting off early.
func (s *Scraper) fetchOGImage(rawURL string) string {
	resp, err := s.get(rawURL, 8*time.Second)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	content := readUntil(resp.Body, [][]byte{[]byte("</head>"), []byte("og:image")}, 500_000)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(content)))
	if err != nil {
		return ""
	}
	if img, ok := doc.Find(`meta[property="og:image"]`).Attr("content"); ok && img != "" {
		return img
	}
	if img, ok := doc.Find(`meta[name="og:image"]`).Attr("content"); ok && img != "" {
		return img
	}
	return ""
}

// readUntil reads from r until one of the stop markers appears in the buffer or
// maxBytes is reached, then returns the buffer.
func readUntil(r io.Reader, stops [][]byte, maxBytes int) []byte {
	var buf []byte
	chunk := make([]byte, 8192)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for _, stop := range stops {
				if containsBytes(buf, stop) {
					return buf
				}
			}
			if len(buf) > maxBytes {
				return buf
			}
		}
		if err != nil {
			return buf
		}
	}
}

func containsBytes(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}

var bodyContainerClasses = []string{
	"article-body", "article__body", "story-body", "post-body",
	"entry-content", "article-content",
}

// fetchArticleBody scrapes readable body text when RSS provides no excerpt.
func (s *Scraper) fetchArticleBody(rawURL string) string {
	resp, err := s.get(rawURL, 6*time.Second)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	content := readUntil(resp.Body, nil, 200_000)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(content)))
	if err != nil {
		return ""
	}
	// Remove noise tags.
	doc.Find("script, style, nav, header, footer, aside, form, ads").Remove()

	// Pick the most specific content container available.
	var body *goquery.Selection
	if sel := doc.Find("article").First(); sel.Length() > 0 {
		body = sel
	} else if sel := doc.Find(`[itemprop="articleBody"]`).First(); sel.Length() > 0 {
		body = sel
	} else if sel := findByClassSubstr(doc, bodyContainerClasses); sel != nil && sel.Length() > 0 {
		body = sel
	} else if sel := doc.Find("main").First(); sel.Length() > 0 {
		body = sel
	}

	var paras *goquery.Selection
	if body != nil {
		paras = body.Find("p")
	} else {
		paras = doc.Find("p")
	}

	var parts []string
	paras.Each(func(_ int, p *goquery.Selection) {
		t := strings.TrimSpace(p.Text())
		if len(t) > 40 {
			parts = append(parts, t)
		}
	})
	text := strings.Join(parts, " ")
	if len(text) > 1000 {
		text = text[:1000]
	}
	return text
}

// findByClassSubstr returns the first element whose class attribute (lowercased)
// contains any of the target substrings.
func findByClassSubstr(doc *goquery.Document, substrs []string) *goquery.Selection {
	var found *goquery.Selection
	doc.Find("[class]").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		class, _ := sel.Attr("class")
		cl := strings.ToLower(class)
		for _, sub := range substrs {
			if strings.Contains(cl, sub) {
				found = sel
				return false
			}
		}
		return true
	})
	return found
}

// enrichArticle resolves the real URL, checks the paywall list, sets the real
// source name, and fetches og:image and body. Returns the (possibly mutated)
// article and whether it was found to be paywalled.
func (s *Scraper) enrichArticle(a *db.Article, isGoogleNews bool) (*db.Article, bool) {
	if isGoogleNews && strings.Contains(a.URL, "news.google.com") {
		resolved := s.resolveURL(a.URL)
		if resolved != "" && !strings.Contains(resolved, "news.google.com") {
			a.URL = resolved
			if a.Source == "" || a.Source == "Google News" {
				a.Source = domainToName(a.URL)
			}
		}
	}

	if isPaywalled(a.URL) {
		return a, true
	}

	realURL := a.URL != "" && !strings.Contains(a.URL, "news.google.com")

	if realURL && len(a.Excerpt) < 100 {
		if body := s.fetchArticleBody(a.URL); body != "" {
			a.Excerpt = body
		}
	}
	if a.ImageURL == "" && realURL {
		a.ImageURL = s.fetchOGImage(a.URL)
	}
	return a, false
}

var (
	wordRe        = regexp.MustCompile(`[a-z0-9]+`)
	titleStopword = map[string]bool{
		"the": true, "a": true, "an": true, "in": true, "on": true, "at": true,
		"to": true, "of": true, "and": true, "for": true, "is": true, "are": true,
		"after": true, "with": true, "who": true, "his": true, "her": true,
		"their": true, "was": true, "has": true, "have": true,
	}
)

// titleWords returns the significant words in a title (lowercased, no stopwords,
// length > 2) as a set.
func titleWords(title string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(title), -1) {
		if !titleStopword[w] && len(w) > 2 {
			out[w] = true
		}
	}
	return out
}

// isNearDuplicateTitle catches different publishers covering the same story
// with reworded headlines via a Jaccard word-overlap ratio ≥ 0.6.
func isNearDuplicateTitle(title string, seen []map[string]bool) bool {
	words := titleWords(title)
	if len(words) == 0 {
		return false
	}
	for _, other := range seen {
		inter, union := 0, len(words)
		for w := range other {
			if words[w] {
				inter++
			} else {
				union++
			}
		}
		if union > 0 && float64(inter)/float64(union) >= 0.6 {
			return true
		}
	}
	return false
}

// interestMatchesCategory does a word-level match between a user interest and a
// feed category, avoiding the substring false-positives of the original test
// ("ai" in "ukraine").
func interestMatchesCategory(interestLower, cat string) bool {
	if interestLower == cat {
		return true
	}
	iw := wordSet(interestLower)
	cw := wordSet(cat)
	if len(iw) == 0 || len(cw) == 0 {
		return false
	}
	return isSubset(iw, cw) || isSubset(cw, iw)
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		out[w] = true
	}
	return out
}

func isSubset(a, b map[string]bool) bool {
	for w := range a {
		if !b[w] {
			return false
		}
	}
	return true
}

// scrapeRSS fetches and parses one category's feed URLs into raw articles
// (pre-enrichment), applying the max-age filter.
func (s *Scraper) scrapeRSS(category string, urls []string) []*db.Article {
	var articles []*db.Article
	parser := &rss.Parser{}
	cutoff := time.Now().UTC().Add(-time.Duration(config.MaxArticleAgeDays) * 24 * time.Hour)

	for _, feedURL := range urls {
		resp, err := s.get(feedURL, 8*time.Second)
		if err != nil {
			continue
		}
		feed, perr := parser.Parse(resp.Body)
		resp.Body.Close()
		if perr != nil || feed == nil {
			continue
		}

		limit := len(feed.Items)
		if limit > 10 {
			limit = 10
		}
		for _, entry := range feed.Items[:limit] {
			excerpt := ""
			if entry.Description != "" {
				excerpt = htmlToText(entry.Description)
				if len(excerpt) > 500 {
					excerpt = excerpt[:500]
				}
			}

			published := ""
			if entry.PubDateParsed != nil {
				// Drop entries we can confirm are too old; keep undated ones.
				if entry.PubDateParsed.Before(cutoff) {
					continue
				}
				published = entry.PubDateParsed.UTC().Format(time.RFC3339)
			} else {
				published = entry.PubDate
			}

			imageURL := extractImage(entry)

			source := ""
			if entry.Source != nil {
				if entry.Source.Title != "" {
					source = entry.Source.Title
				} else {
					source = entry.Source.URL
				}
			}
			if source == "" {
				source = feed.Title
			}

			title := entry.Title
			if source != "" && strings.HasSuffix(title, " - "+source) {
				title = strings.TrimSuffix(title, " - "+source)
			}

			articles = append(articles, &db.Article{
				URL:         entry.Link,
				SourceURL:   entry.Link,
				Title:       title,
				Source:      source,
				Category:    category,
				Excerpt:     excerpt,
				PublishedAt: published,
				ImageURL:    imageURL,
				// Summary stays NULL until the summarizer runs.
			})
		}
		time.Sleep(200 * time.Millisecond)
	}
	return articles
}

// extractImage pulls an image URL from media:thumbnail, media:content, or an
// image enclosure, mirroring feedparser's preference order.
func extractImage(entry *rss.Item) string {
	if entry.Extensions != nil {
		if media, ok := entry.Extensions["media"]; ok {
			for _, key := range []string{"thumbnail", "content"} {
				if exts, ok := media[key]; ok {
					for _, e := range exts {
						if u := e.Attrs["url"]; u != "" {
							return u
						}
					}
				}
			}
		}
	}
	for _, enc := range entry.Enclosures {
		if strings.HasPrefix(enc.Type, "image") && enc.URL != "" {
			return enc.URL
		}
	}
	return ""
}

// htmlToText strips HTML markup and returns the text content.
func htmlToText(html string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return html
	}
	return doc.Text()
}

// ScrapeAll scrapes feeds for the given interest categories, deduping across
// sources and enriching new articles concurrently.
func (s *Scraper) ScrapeAll(interests []string) []*db.Article {
	var allArticles []*db.Article
	seenURLs := map[string]bool{}

	for _, interest := range interests {
		interestLower := strings.ToLower(interest)

		type catURLs struct {
			cat  string
			urls []string
		}
		var matched []catURLs
		for cat, urls := range config.RSSFeeds {
			if interestMatchesCategory(interestLower, cat) {
				matched = append(matched, catURLs{cat, urls})
			}
		}
		if len(matched) == 0 {
			// Not a predefined category — build a Google News search feed for it
			// rather than silently falling back to "world news".
			matched = []catURLs{{
				cat:  interestLower,
				urls: []string{config.GN + url.QueryEscape(strings.TrimSpace(interest))},
			}}
		}

		for _, m := range matched {
			cat, urls := m.cat, m.urls
			isGoogle := config.GoogleNewsCategories[cat] || anyContains(urls, "news.google.com")
			articles := s.scrapeRSS(cat, urls)

			negativeKeywords := config.CategoryNegativeKeywords[cat]
			keywords := config.CategoryKeywords[cat]
			seenSources := map[string]int{}
			var seenTitles []map[string]bool

			var candidates []*db.Article
			for _, a := range articles {
				if a.URL == "" || seenURLs[a.URL] {
					continue
				}
				text := strings.ToLower(a.Title + " " + a.Excerpt)
				if !isGoogle && len(keywords) > 0 {
					if !anyKeyword(text, keywords) {
						continue
					}
				}
				if len(negativeKeywords) > 0 && anyKeyword(text, negativeKeywords) {
					continue
				}
				if seenSources[a.Source] >= 2 {
					continue
				}
				if isNearDuplicateTitle(a.Title, seenTitles) {
					continue
				}
				seenSources[a.Source]++
				seenTitles = append(seenTitles, titleWords(a.Title))
				candidates = append(candidates, a)
			}

			// Reuse cached rows for candidates already scraped in a prior run,
			// so repeat refreshes skip re-decoding and re-fetching.
			sourceURLs := make([]string, len(candidates))
			for i, c := range candidates {
				sourceURLs[i] = c.SourceURL
			}
			known, _ := s.db.GetArticlesBySourceURLs(sourceURLs)

			var toEnrich []*db.Article
			for _, c := range candidates {
				cached, ok := known[c.SourceURL]
				if ok && cached.URL != "" && !seenURLs[cached.URL] {
					seenURLs[cached.URL] = true
					allArticles = append(allArticles, &db.Article{
						URL:         cached.URL,
						SourceURL:   c.SourceURL,
						Title:       cached.Title,
						Source:      cached.Source,
						Category:    cat,
						Excerpt:     cached.Excerpt,
						Summary:     cached.Summary,
						ImageURL:    cached.ImageURL,
						PublishedAt: cached.PublishedAt,
					})
				} else if !ok {
					toEnrich = append(toEnrich, c)
				}
			}

			// Enrich new articles concurrently (max 3 in flight), then collect
			// non-paywalled results under the dedup lock.
			enriched := s.enrichConcurrent(toEnrich, isGoogle)
			for _, e := range enriched {
				if e.URL != "" && !seenURLs[e.URL] {
					seenURLs[e.URL] = true
					allArticles = append(allArticles, e)
				}
			}
		}
	}
	return allArticles
}

// enrichConcurrent enriches articles with a bounded worker pool, dropping
// paywalled ones. Order is not preserved (matches the Python as_completed loop).
func (s *Scraper) enrichConcurrent(articles []*db.Article, isGoogle bool) []*db.Article {
	if len(articles) == 0 {
		return nil
	}
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var out []*db.Article

	for _, a := range articles {
		wg.Add(1)
		go func(a *db.Article) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			enriched, paywalled := s.enrichArticle(a, isGoogle)
			if paywalled {
				return
			}
			mu.Lock()
			out = append(out, enriched)
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	return out
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func anyKeyword(text string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// SaveArticles persists articles, returning the ids of newly inserted rows.
func (s *Scraper) SaveArticles(articles []*db.Article) []int64 {
	var ids []int64
	for _, a := range articles {
		id, err := s.db.SaveArticle(a)
		if err == nil && id != 0 {
			ids = append(ids, id)
		}
	}
	return ids
}
