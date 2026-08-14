// Package server exposes the news aggregator over HTTP: web onboarding, the
// JSON API for users/articles/digests/costs, and the feed UI. Ported from
// api.py (Flask) to net/http.
package server

import (
	"embed"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"news-aggregator/config"
	"news-aggregator/db"
	"news-aggregator/scraper"
	"news-aggregator/summarizer"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server holds shared dependencies and the in-process caches that the Python
// version kept as module globals (refresh status + briefing cache). A single
// worker with shared memory is assumed, matching the deploy note in the
// original Dockerfile.
type Server struct {
	cfg  *config.Config
	db   *db.DB
	scr  *scraper.Scraper
	sum  *summarizer.Summarizer
	tmpl *template.Template
	mux  *http.ServeMux

	refreshMu     sync.Mutex
	refreshStatus map[string]string // token -> "running" | "done"

	briefingMu    sync.Mutex
	briefingCache map[string][]summarizer.Bullet
}

// New wires up the server and its routes.
func New(cfg *config.Config, database *db.DB, scr *scraper.Scraper, sum *summarizer.Summarizer) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"has": func(list []string, v string) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		},
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:           cfg,
		db:            database,
		scr:           scr,
		sum:           sum,
		tmpl:          tmpl,
		mux:           http.NewServeMux(),
		refreshStatus: map[string]string{},
		briefingCache: map[string][]summarizer.Bullet{},
	}
	s.routes()
	return s, nil
}

// Handler returns the root HTTP handler with CORS applied.
func (s *Server) Handler() http.Handler {
	return s.withCORS(s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.health)
	s.mux.HandleFunc("GET /", s.landing)
	s.mux.HandleFunc("GET /signup", s.signupPage)
	s.mux.HandleFunc("POST /signup", s.signupSubmit)
	s.mux.HandleFunc("POST /login", s.loginSubmit)

	s.mux.HandleFunc("POST /users", s.requireAccessCode(s.createUser))
	s.mux.HandleFunc("GET /users/{token}", s.getUser)
	s.mux.HandleFunc("PUT /users/{token}", s.updateUser)
	s.mux.HandleFunc("DELETE /users/{token}", s.requireAccessCode(s.deleteUser))
	s.mux.HandleFunc("GET /users/{token}/digest", s.getDigest)

	s.mux.HandleFunc("POST /digest/run", s.requireAccessCode(s.runAllDigests))

	s.mux.HandleFunc("GET /articles", s.getArticles)

	s.mux.HandleFunc("GET /costs", s.requireAccessCode(s.getCosts))
	s.mux.HandleFunc("GET /costs/total", s.requireAccessCode(s.getTotalCosts))

	s.mux.HandleFunc("GET /feed/{token}", s.feedUI)
	s.mux.HandleFunc("GET /feed/{token}/data", s.feedData)
	s.mux.HandleFunc("POST /feed/{token}/refresh", s.refreshFeed)
	s.mux.HandleFunc("GET /feed/{token}/refresh/status", s.refreshStatusHandler)
}

// --- middleware ---

func (s *Server) withCORS(next http.Handler) http.Handler {
	allowAll := len(s.cfg.AllowedOrigins) == 1 && s.cfg.AllowedOrigins[0] == "*"
	allowed := map[string]bool{}
	for _, o := range s.cfg.AllowedOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Access-Code")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAccessCode gates admin/cost endpoints with ACCESS_CODE, supplied via
// the X-Access-Code header or ?code= query param. No-op when unset.
func (s *Server) requireAccessCode(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AccessCode != "" {
			supplied := r.Header.Get("X-Access-Code")
			if supplied == "" {
				supplied = r.URL.Query().Get("code")
			}
			if supplied != s.cfg.AccessCode {
				s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

// --- helpers ---

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

// externalURL builds an absolute URL for a path, honoring the reverse-proxy
// X-Forwarded-Proto header (Railway/Render terminate TLS at the edge), so the
// shareable /feed/<token> link uses https where appropriate.
func (s *Server) externalURL(r *http.Request, path string) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host + path
}

// articleSummary returns the summary string or nil (JSON null) when unset,
// preserving the NULL-vs-empty distinction in API responses.
func articleSummary(a *db.Article) any {
	if a.Summary.Valid {
		return a.Summary.String
	}
	return nil
}

// fullArticleMap serializes every column of an article, matching the Python
// endpoints that return dict(row).
func fullArticleMap(a *db.Article) map[string]any {
	return map[string]any{
		"id":           a.ID,
		"url":          a.URL,
		"title":        a.Title,
		"source":       a.Source,
		"category":     a.Category,
		"excerpt":      a.Excerpt,
		"summary":      articleSummary(a),
		"image_url":    a.ImageURL,
		"source_url":   a.SourceURL,
		"published_at": a.PublishedAt,
		"scraped_at":   a.ScrapedAt,
	}
}

// userMap serializes a user the way the Python jsonify(user) did.
func userMap(u *db.User) map[string]any {
	return map[string]any{
		"id":           u.ID,
		"phone_number": u.PhoneNumber,
		"name":         u.Name,
		"interests":    u.Interests,
		"sources":      u.Sources,
		"subreddits":   u.Subreddits,
		"location":     u.Location,
		"active":       u.Active,
		"created_at":   u.CreatedAt,
		"token":        u.Token,
	}
}

// interestsOrDefault falls back to ["world news"] when a user has no interests.
func interestsOrDefault(in []string) []string {
	if len(in) == 0 {
		return []string{"world news"}
	}
	return in
}

// --- health ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// --- web onboarding ---

func (s *Server) landing(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/signup", http.StatusFound)
}

type signupData struct {
	Categories        []string
	RequireCode       bool
	Error             string
	Name              string
	Location          string
	SelectedInterests []string
	CustomInterest    string
	LoginError        string
	LoginName         string
}

func (s *Server) renderSignup(w http.ResponseWriter, status int, data signupData) {
	data.Categories = config.CategoryOrder
	data.RequireCode = s.cfg.AccessCode != ""
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, "signup.html", data); err != nil {
		log.Printf("render signup: %v", err)
	}
}

func (s *Server) signupPage(w http.ResponseWriter, r *http.Request) {
	s.renderSignup(w, http.StatusOK, signupData{})
}

func (s *Server) signupSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	location := strings.TrimSpace(r.FormValue("location"))
	customInterest := strings.TrimSpace(r.FormValue("custom_interest"))

	interests := append([]string{}, r.Form["interests"]...)
	for _, c := range strings.Split(customInterest, ",") {
		if s := strings.TrimSpace(c); s != "" {
			interests = append(interests, s)
		}
	}

	if s.cfg.AccessCode != "" && strings.TrimSpace(r.FormValue("code")) != s.cfg.AccessCode {
		s.renderSignup(w, http.StatusOK, signupData{
			Error:             "Invalid access code.",
			Name:              name,
			Location:          location,
			SelectedInterests: interests,
			CustomInterest:    customInterest,
		})
		return
	}

	user, err := s.db.CreateUser(name)
	if err != nil {
		http.Error(w, "could not create user", http.StatusInternalServerError)
		return
	}
	if len(interests) == 0 {
		interests = []string{"world news"}
	}
	if err := s.db.UpdateUser(user.Token, map[string]any{"interests": interests, "location": location}); err != nil {
		log.Printf("signup update: %v", err)
	}

	feedURL := s.externalURL(r, "/feed/"+user.Token)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "link.html", map[string]any{"Name": name, "FeedURL": feedURL}); err != nil {
		log.Printf("render link: %v", err)
	}
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	code := strings.TrimSpace(r.FormValue("code"))

	if s.cfg.AccessCode != "" && code != s.cfg.AccessCode {
		s.renderSignup(w, http.StatusOK, signupData{LoginError: "Invalid access code.", LoginName: name})
		return
	}

	var user *db.User
	if name != "" {
		u, err := s.db.GetUserByName(name)
		if err != nil {
			log.Printf("login lookup: %v", err)
		}
		user = u
	}
	if user == nil {
		s.renderSignup(w, http.StatusOK, signupData{
			LoginError: "We couldn't find an account with that name.", LoginName: name,
		})
		return
	}
	http.Redirect(w, r, "/feed/"+user.Token, http.StatusFound)
}

// --- user endpoints ---

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	user, err := s.db.CreateUser(strings.TrimSpace(body.Name))
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not create user"})
		return
	}
	s.writeJSON(w, http.StatusOK, userMap(user))
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	user, err := s.db.GetUser(r.PathValue("token"))
	if err != nil || user == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "User not found"})
		return
	}
	s.writeJSON(w, http.StatusOK, userMap(user))
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	user, err := s.db.GetUser(token)
	if err != nil || user == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "User not found"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	allowed := map[string]bool{"name": true, "interests": true, "sources": true, "subreddits": true, "location": true}
	updates := map[string]any{}
	for k, v := range body {
		if !allowed[k] {
			continue
		}
		// interests/sources/subreddits arrive as JSON arrays -> []string.
		if arr, ok := v.([]any); ok {
			strs := make([]string, 0, len(arr))
			for _, e := range arr {
				if s, ok := e.(string); ok {
					strs = append(strs, s)
				}
			}
			updates[k] = strs
		} else {
			updates[k] = v
		}
	}
	if err := s.db.UpdateUser(token, updates); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "update failed"})
		return
	}
	updated, _ := s.db.GetUser(token)
	s.writeJSON(w, http.StatusOK, userMap(updated))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	ok, err := s.db.DeleteUser(r.PathValue("token"))
	if err != nil || !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "User not found"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// --- digest endpoints ---

func (s *Server) getDigest(w http.ResponseWriter, r *http.Request) {
	user, err := s.db.GetUser(r.PathValue("token"))
	if err != nil || user == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "User not found"})
		return
	}
	interests := interestsOrDefault(user.Interests)

	articles := s.scr.ScrapeAll(interests)
	s.scr.SaveArticles(articles)

	unsent, _ := s.db.GetArticlesForUser(user.ID, interests, 10)
	if len(unsent) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{"briefing": "No new articles today.", "articles": []any{}})
		return
	}

	summarized := s.sum.SummarizeBatch(unsent)
	briefing := s.sum.WriteMorningBriefing(summarized, user.Name)
	ids := articleIDs(summarized)
	if err := s.db.MarkArticlesSent(user.ID, ids); err != nil {
		log.Printf("mark sent: %v", err)
	}

	arts := make([]map[string]any, 0, len(summarized))
	for _, a := range summarized {
		arts = append(arts, map[string]any{
			"title":    a.Title,
			"category": a.Category,
			"summary":  articleSummary(a),
			"url":      a.URL,
			"source":   a.Source,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"user":     userDisplay(user),
		"briefing": briefing,
		"articles": arts,
	})
}

func (s *Server) runAllDigests(w http.ResponseWriter, r *http.Request) {
	users, _ := s.db.GetAllUsers()
	if len(users) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{"message": "No users found."})
		return
	}

	// Dedupe scraping across all users' interests.
	interestSet := map[string]bool{}
	for _, u := range users {
		for _, i := range u.Interests {
			interestSet[i] = true
		}
	}
	allInterests := make([]string, 0, len(interestSet))
	for i := range interestSet {
		allInterests = append(allInterests, i)
	}
	articles := s.scr.ScrapeAll(allInterests)
	s.scr.SaveArticles(articles)

	results := make([]map[string]any, 0, len(users))
	for _, user := range users {
		interests := interestsOrDefault(user.Interests)
		unsent, _ := s.db.GetArticlesForUser(user.ID, interests, 10)
		if len(unsent) > 0 {
			summarized := s.sum.SummarizeBatch(unsent)
			s.sum.WriteMorningBriefing(summarized, user.Name)
			_ = s.db.MarkArticlesSent(user.ID, articleIDs(summarized))
			results = append(results, map[string]any{"user": userDisplay(user), "articles_sent": len(summarized)})
		} else {
			results = append(results, map[string]any{"user": userDisplay(user), "articles_sent": 0})
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// --- articles ---

func (s *Server) getArticles(w http.ResponseWriter, r *http.Request) {
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	rows, err := s.db.GetArticlesByCategory(category, limit)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "query failed"})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, fullArticleMap(a))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// --- costs ---

func (s *Server) getCosts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.GetCostSummary()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "query failed"})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{"service": c.Service, "total": c.Total, "date": c.Date})
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) getTotalCosts(w http.ResponseWriter, r *http.Request) {
	grand, breakdown, err := s.db.GetMonthlyCosts()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "query failed"})
		return
	}
	bd := make([]map[string]any, 0, len(breakdown))
	for _, b := range breakdown {
		bd = append(bd, map[string]any{
			"service":      b.Service,
			"total_usd":    b.TotalUSD,
			"api_calls":    b.APICalls,
			"total_tokens": b.TotalTokens,
			"month":        b.Month,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"month":           "current",
		"grand_total_usd": round6(grand),
		"breakdown":       bd,
	})
}

// --- feed UI ---

func (s *Server) feedUI(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	user, err := s.db.GetUser(token)
	if err != nil || user == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "feed.html", map[string]any{"Token": token}); err != nil {
		log.Printf("render feed: %v", err)
	}
}

func (s *Server) feedData(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	user, err := s.db.GetUser(token)
	if err != nil || user == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "User not found"})
		return
	}
	interests := interestsOrDefault(user.Interests)

	// Fetch top articles per category from the DB (scraping happens via the
	// /refresh endpoint). Over-fetch since image-less articles are filtered
	// out of the card grid below.
	var all []*db.Article
	seenIDs := map[int64]bool{}
	for _, interest := range interests {
		rows, err := s.db.GetArticlesByCategory(interest, 15)
		if err != nil {
			continue
		}
		for _, a := range rows {
			if !seenIDs[a.ID] {
				seenIDs[a.ID] = true
				all = append(all, a)
			}
		}
	}

	// Use whatever summaries already exist; deliberately DON'T summarize here —
	// synchronous Claude calls on page load caused worker timeouts.
	briefing := s.cachedBriefing(user, all)

	costToday, _ := s.db.GetCostToday()

	// Only articles with a real image make it into the card grid.
	arts := make([]map[string]any, 0, len(all))
	for _, a := range all {
		if a.ImageURL == "" {
			continue
		}
		arts = append(arts, map[string]any{
			"id":        a.ID,
			"title":     a.Title,
			"category":  a.Category,
			"summary":   articleSummary(a),
			"url":       a.URL,
			"source":    a.Source,
			"image_url": a.ImageURL,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"user":       userDisplay(user),
		"briefing":   briefing,
		"articles":   arts,
		"cost_today": costToday,
	})
}

// cachedBriefing returns the briefing for this user + article set, computing and
// caching it on a miss. Viewing the web feed must not mark articles "sent".
func (s *Server) cachedBriefing(user *db.User, articles []*db.Article) []summarizer.Bullet {
	ids := make([]int64, 0, len(articles))
	for _, a := range articles {
		if a.ID != 0 {
			ids = append(ids, a.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var sb strings.Builder
	sb.WriteString(strconv.FormatInt(user.ID, 10))
	sb.WriteByte(':')
	for _, id := range ids {
		sb.WriteString(strconv.FormatInt(id, 10))
		sb.WriteByte(',')
	}
	key := sb.String()

	s.briefingMu.Lock()
	if b, ok := s.briefingCache[key]; ok {
		s.briefingMu.Unlock()
		return b
	}
	s.briefingMu.Unlock()

	briefing := s.sum.WriteMorningBriefing(articles, user.Name)

	s.briefingMu.Lock()
	if len(s.briefingCache) > 100 {
		s.briefingCache = map[string][]summarizer.Bullet{}
	}
	s.briefingCache[key] = briefing
	s.briefingMu.Unlock()
	return briefing
}

func (s *Server) refreshFeed(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	user, err := s.db.GetUser(token)
	if err != nil || user == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "User not found"})
		return
	}

	// Check-and-set under the lock so two near-simultaneous clicks don't both
	// launch a scrape+Claude run.
	s.refreshMu.Lock()
	if s.refreshStatus[token] == "running" {
		s.refreshMu.Unlock()
		s.writeJSON(w, http.StatusOK, map[string]any{"status": "already_running"})
		return
	}
	s.refreshStatus[token] = "running"
	s.refreshMu.Unlock()

	interests := interestsOrDefault(user.Interests)

	go func() {
		defer func() {
			s.refreshMu.Lock()
			s.refreshStatus[token] = "done"
			s.refreshMu.Unlock()
		}()
		articles := s.scr.ScrapeAll(interests)
		s.scr.SaveArticles(articles)
		// Summarize in the background so /data never makes slow Claude calls on
		// page load; summaries are persisted for a later /data read.
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("Background summarize failed: %v", rec)
				}
			}()
			s.sum.SummarizeBatch(articles)
		}()
	}()

	s.writeJSON(w, http.StatusOK, map[string]any{"status": "started"})
}

func (s *Server) refreshStatusHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	s.refreshMu.Lock()
	status := s.refreshStatus[token]
	s.refreshMu.Unlock()
	if status == "" {
		status = "idle"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

// --- small helpers ---

func articleIDs(articles []*db.Article) []int64 {
	ids := make([]int64, 0, len(articles))
	for _, a := range articles {
		ids = append(ids, a.ID)
	}
	return ids
}

// userDisplay is the user's name, falling back to their token.
func userDisplay(u *db.User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Token
}

// round6 rounds to 6 decimal places, matching Python's round(x, 6).
func round6(x float64) float64 {
	const p = 1e6
	if x >= 0 {
		return float64(int64(x*p+0.5)) / p
	}
	return float64(int64(x*p-0.5)) / p
}
