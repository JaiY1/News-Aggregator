package scraper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// resolveURL decodes a Google News RSS redirect URL to the real article URL.
// It reimplements the googlenewsdecoder "new_decoderv1" flow: extract the
// article id, scrape a signature/timestamp from the article page, then POST to
// Google's batchexecute endpoint which returns the destination URL. On any
// failure it returns the input URL unchanged, matching the Python behavior.
func (s *Scraper) resolveURL(rawURL string) string {
	if !strings.Contains(rawURL, "news.google.com") {
		return rawURL
	}
	id, ok := googleNewsID(rawURL)
	if !ok {
		return rawURL
	}
	sig, ts, ok := s.gnewsDecodingParams(id)
	if !ok {
		return rawURL
	}
	if decoded, ok := s.gnewsDecode(id, ts, sig); ok && decoded != "" {
		return decoded
	}
	return rawURL
}

// googleNewsID extracts the opaque article id from a Google News URL of the
// form .../articles/<id> or .../read/<id>.
func googleNewsID(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host != "news.google.com" {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", false
	}
	kind := parts[len(parts)-2]
	if kind != "articles" && kind != "read" {
		return "", false
	}
	return parts[len(parts)-1], true
}

// gnewsDecodingParams fetches the article page and extracts the signature
// (data-n-a-sg) and timestamp (data-n-a-ts) needed for the decode request.
func (s *Scraper) gnewsDecodingParams(id string) (sig, ts string, ok bool) {
	resp, err := s.get("https://news.google.com/articles/"+id, 10*time.Second)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", "", false
	}
	div := doc.Find("c-wiz > div").First()
	sig, _ = div.Attr("data-n-a-sg")
	ts, _ = div.Attr("data-n-a-ts")
	if sig == "" || ts == "" {
		return "", "", false
	}
	return sig, ts, true
}

// gnewsDecode POSTs to the batchexecute endpoint and parses the destination URL
// out of the response.
func (s *Scraper) gnewsDecode(id, ts, sig string) (string, bool) {
	inner := `["garturlreq",[["X","X",["X","X"],null,null,1,1,"US:en",null,1,null,null,null,null,null,0,1],"X","X",1,[1,1,1],1,1,null,0,0,null,0],"` +
		id + `",` + ts + `,"` + sig + `"]`

	// Build [[["Fbv4je", inner]]] and JSON-encode so the inner string is escaped.
	freqBytes, err := json.Marshal([]any{[]any{[]any{"Fbv4je", inner}}})
	if err != nil {
		return "", false
	}
	form := url.Values{}
	form.Set("f.req", string(freqBytes))

	// The shared client has no global timeout (each request carries its own via
	// context) — without this, a hung batchexecute POST would block an
	// enrichment worker indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://news.google.com/_/DotsSplashUi/data/batchexecute",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	return parseBatchExecute(string(body))
}

// parseBatchExecute extracts the decoded URL from a batchexecute response. The
// body is an XSSI-guarded payload: ")]}'" then a blank line then a JSON array
// whose "wrb.fr" row carries, at index 2, a JSON string ["garturlres","<url>"].
func parseBatchExecute(body string) (string, bool) {
	idx := strings.Index(body, "\n\n")
	if idx < 0 {
		return "", false
	}
	remainder := strings.TrimSpace(body[idx+2:])

	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(remainder), &rows); err != nil {
		return "", false
	}
	for _, raw := range rows {
		var row []json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil || len(row) < 3 {
			continue
		}
		var tag string
		if json.Unmarshal(row[0], &tag) != nil || tag != "wrb.fr" {
			continue
		}
		var payload string
		if json.Unmarshal(row[2], &payload) != nil || payload == "" {
			continue
		}
		var parsed []any
		if json.Unmarshal([]byte(payload), &parsed) != nil || len(parsed) < 2 {
			continue
		}
		if urlStr, ok := parsed[1].(string); ok {
			return urlStr, true
		}
	}
	return "", false
}
