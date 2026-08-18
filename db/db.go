// Package db is the SQLite persistence layer: users, articles, per-user digest
// tracking, and API cost logging. Ported from database.py.
package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// DB wraps a *sql.DB connection pool to the SQLite database.
type DB struct {
	sql *sql.DB
}

// User is one subscriber. Summary of the token model: signup no longer
// requires a phone number, so the opaque Token is the shareable-link
// credential; PhoneNumber stays in the schema, unused, for future SMS work.
type User struct {
	ID          int64    `json:"id"`
	PhoneNumber string   `json:"phone_number"`
	Name        string   `json:"name"`
	Interests   []string `json:"interests"`
	Sources     []string `json:"sources"`
	Subreddits  []string `json:"subreddits"`
	Location    string   `json:"location"`
	Active      int      `json:"active"`
	CreatedAt   string   `json:"created_at"`
	Token       string   `json:"token"`
}

// Article is a scraped news item, deduped by URL.
//
// Summary is nullable on purpose: NULL means "never summarized", while an
// empty string means "tried, but the article had no usable content" — the
// summarizer treats the latter as done so it isn't re-sent to Claude every run.
type Article struct {
	ID          int64          `json:"id"`
	URL         string         `json:"url"`
	Title       string         `json:"title"`
	Source      string         `json:"source"`
	Category    string         `json:"category"`
	Excerpt     string         `json:"excerpt"`
	Summary     sql.NullString `json:"-"`
	ImageURL    string         `json:"image_url"`
	SourceURL   string         `json:"source_url"`
	PublishedAt string         `json:"published_at"`
	ScrapedAt   string         `json:"scraped_at"`
}

// CostRow is one grouped row of the cost summary (service + day).
type CostRow struct {
	Service string  `json:"service"`
	Total   float64 `json:"total"`
	Date    string  `json:"date"`
}

// Open opens (and pings) the SQLite database at path.
func Open(path string) (*DB, error) {
	// busy_timeout keeps concurrent writers from failing outright with
	// "database is locked"; SQLite serializes writes and this waits instead.
	// NOTE: modernc.org/sqlite takes pragmas via _pragma=... — the mattn-style
	// _busy_timeout=5000 param is silently ignored (timeout stays 0).
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}
	return &DB{sql: sqlDB}, nil
}

// Close closes the underlying connection pool.
func (d *DB) Close() error { return d.sql.Close() }

// Init creates the schema and indexes if they don't already exist. The full
// column set (including columns added by ALTER-TABLE migrations in the Python
// version) is declared up front, so a fresh database matches the final schema.
func (d *DB) Init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			phone_number TEXT UNIQUE,
			name TEXT,
			interests TEXT DEFAULT '[]',
			sources TEXT DEFAULT '[]',
			subreddits TEXT DEFAULT '[]',
			location TEXT,
			active INTEGER DEFAULT 1,
			created_at TEXT DEFAULT (datetime('now')),
			token TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_token ON users(token)`,
		`CREATE TABLE IF NOT EXISTS articles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url TEXT UNIQUE,
			title TEXT,
			source TEXT,
			category TEXT,
			excerpt TEXT,
			summary TEXT,
			image_url TEXT,
			source_url TEXT,
			published_at TEXT,
			scraped_at TEXT DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_articles_source_url ON articles(source_url)`,
		`CREATE TABLE IF NOT EXISTS user_digests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER,
			article_id INTEGER,
			sent_at TEXT DEFAULT (datetime('now')),
			UNIQUE(user_id, article_id)
		)`,
		`CREATE TABLE IF NOT EXISTS api_costs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			service TEXT,
			operation TEXT,
			units REAL,
			cost_usd REAL,
			logged_at TEXT DEFAULT (datetime('now'))
		)`,
	}
	for _, s := range stmts {
		if _, err := d.sql.Exec(s); err != nil {
			return fmt.Errorf("init: %w", err)
		}
	}
	return nil
}

// newToken mirrors secrets.token_urlsafe(8): 8 random bytes, base64url, no pad.
func newToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func toJSON(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func fromJSON(s string) []string {
	if s == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []string{}
	}
	return out
}

// --- User functions ---

// CreateUser inserts a user identified by an opaque token and returns it.
func (d *DB) CreateUser(name string) (*User, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	if _, err := d.sql.Exec(
		`INSERT INTO users (token, name) VALUES (?, ?)`, token, name,
	); err != nil {
		return nil, err
	}
	return d.GetUser(token)
}

// DeleteUser removes a user and their digest history. Returns false if the
// token didn't exist.
func (d *DB) DeleteUser(token string) (bool, error) {
	var id int64
	err := d.sql.QueryRow(`SELECT id FROM users WHERE token = ?`, token).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := d.sql.Exec(`DELETE FROM user_digests WHERE user_id = ?`, id); err != nil {
		return false, err
	}
	if _, err := d.sql.Exec(`DELETE FROM users WHERE token = ?`, token); err != nil {
		return false, err
	}
	return true, nil
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var phone, name, location, token sql.NullString
	var interests, sources, subreddits, createdAt sql.NullString
	if err := row.Scan(
		&u.ID, &phone, &name, &interests, &sources, &subreddits,
		&location, &u.Active, &createdAt, &token,
	); err != nil {
		return nil, err
	}
	u.PhoneNumber = phone.String
	u.Name = name.String
	u.Location = location.String
	u.Token = token.String
	u.CreatedAt = createdAt.String
	u.Interests = fromJSON(interests.String)
	u.Sources = fromJSON(sources.String)
	u.Subreddits = fromJSON(subreddits.String)
	return &u, nil
}

const userCols = `id, phone_number, name, interests, sources, subreddits, location, active, created_at, token`

// GetUser looks up a user by token.
func (d *DB) GetUser(token string) (*User, error) {
	row := d.sql.QueryRow(`SELECT `+userCols+` FROM users WHERE token = ?`, token)
	u, err := scanUser(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// GetUserByName does a case-insensitive name lookup for the login form. Names
// aren't unique, so this returns the most recently created active match.
func (d *DB) GetUserByName(name string) (*User, error) {
	row := d.sql.QueryRow(
		`SELECT `+userCols+` FROM users WHERE LOWER(name) = LOWER(?) AND active = 1 ORDER BY created_at DESC LIMIT 1`,
		name,
	)
	u, err := scanUser(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// GetAllUsers returns every active user.
func (d *DB) GetAllUsers() ([]*User, error) {
	rows, err := d.sql.Query(`SELECT ` + userCols + ` FROM users WHERE active = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// updatableUserCols whitelists which columns UpdateUser may set, so the
// dynamic column name can never come from untrusted input.
var updatableUserCols = map[string]bool{
	"name": true, "interests": true, "sources": true,
	"subreddits": true, "location": true,
}

// UpdateUser applies the given column updates for a user. List values (for the
// JSON-array columns) are passed as []string and marshaled; scalar values as
// string. Unknown columns are ignored.
func (d *DB) UpdateUser(token string, updates map[string]any) error {
	for key, value := range updates {
		if !updatableUserCols[key] {
			continue
		}
		var stored any
		switch v := value.(type) {
		case []string:
			stored = toJSON(v)
		default:
			stored = v
		}
		if _, err := d.sql.Exec(
			fmt.Sprintf(`UPDATE users SET %s = ? WHERE token = ?`, key), stored, token,
		); err != nil {
			return err
		}
	}
	return nil
}

// --- Article functions ---

// SaveArticle inserts an article (ignoring duplicates by URL) and back-fills a
// missing image_url. Returns the new row id, or 0 if the row already existed.
func (d *DB) SaveArticle(a *Article) (int64, error) {
	res, err := d.sql.Exec(`
		INSERT OR IGNORE INTO articles
			(url, title, source, category, excerpt, summary, image_url, source_url, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.URL, a.Title, a.Source, a.Category, a.Excerpt,
		nullable(a.Summary), a.ImageURL, a.SourceURL, a.PublishedAt,
	)
	if err != nil {
		return 0, err
	}
	if a.ImageURL != "" {
		// Patch image_url if it was missing before.
		_, _ = d.sql.Exec(
			`UPDATE articles SET image_url = ? WHERE url = ? AND (image_url IS NULL OR image_url = '')`,
			a.ImageURL, a.URL,
		)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// LastInsertId is nonzero only when a row was actually inserted.
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return 0, nil
	}
	return id, nil
}

func nullable(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}

// NullStr wraps a string as a valid (non-NULL) sql.NullString.
func NullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

const articleCols = `id, url, title, source, category, excerpt, summary, image_url, source_url, published_at, scraped_at`

func scanArticle(row interface{ Scan(...any) error }) (*Article, error) {
	var a Article
	var url, title, source, category, excerpt, imageURL, sourceURL, publishedAt, scrapedAt sql.NullString
	if err := row.Scan(
		&a.ID, &url, &title, &source, &category, &excerpt,
		&a.Summary, &imageURL, &sourceURL, &publishedAt, &scrapedAt,
	); err != nil {
		return nil, err
	}
	a.URL = url.String
	a.Title = title.String
	a.Source = source.String
	a.Category = category.String
	a.Excerpt = excerpt.String
	a.ImageURL = imageURL.String
	a.SourceURL = sourceURL.String
	a.PublishedAt = publishedAt.String
	a.ScrapedAt = scrapedAt.String
	return &a, nil
}

// GetArticlesBySourceURLs looks up already-scraped articles by their raw feed
// URL (pre-decode/pre-enrichment), keyed by source_url, so a re-scrape can skip
// re-fetching body/image/decoding for ones we already have.
func (d *DB) GetArticlesBySourceURLs(sourceURLs []string) (map[string]*Article, error) {
	out := make(map[string]*Article)
	if len(sourceURLs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sourceURLs)), ",")
	args := make([]any, len(sourceURLs))
	for i, s := range sourceURLs {
		args[i] = s
	}
	rows, err := d.sql.Query(
		`SELECT `+articleCols+` FROM articles WHERE source_url IN (`+placeholders+`)`, args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out[a.SourceURL] = a
	}
	return out, rows.Err()
}

// SaveSummary persists a generated summary back to the article row.
func (d *DB) SaveSummary(url, summary string) error {
	_, err := d.sql.Exec(`UPDATE articles SET summary = ? WHERE url = ?`, summary, url)
	return err
}

// escapeLike escapes LIKE wildcards in user-supplied text so an interest
// containing % or _ matches literally instead of acting as a pattern.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func escapeLike(s string) string { return likeEscaper.Replace(s) }

// GetArticlesForUser returns up to limit articles not yet sent to the user that
// match any of their interests, newest first.
func (d *DB) GetArticlesForUser(userID int64, interests []string, limit int) ([]*Article, error) {
	if len(interests) == 0 {
		return nil, nil
	}
	clauses := make([]string, len(interests))
	args := make([]any, 0, len(interests)+2)
	for i, in := range interests {
		clauses[i] = `LOWER(category) LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(strings.ToLower(in))+"%")
	}
	args = append(args, userID, limit)
	query := `
		SELECT ` + articleCols + ` FROM articles a
		WHERE (` + strings.Join(clauses, " OR ") + `)
		AND a.id NOT IN (SELECT article_id FROM user_digests WHERE user_id = ?)
		ORDER BY a.scraped_at DESC
		LIMIT ?`
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetArticlesByCategory returns up to limit articles whose category matches the
// substring, newest first. Used by GET /articles and the feed data endpoint.
func (d *DB) GetArticlesByCategory(category string, limit int) ([]*Article, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if category == "" {
		rows, err = d.sql.Query(
			`SELECT `+articleCols+` FROM articles ORDER BY scraped_at DESC LIMIT ?`, limit,
		)
	} else {
		rows, err = d.sql.Query(
			`SELECT `+articleCols+` FROM articles WHERE LOWER(category) LIKE ? ESCAPE '\' ORDER BY scraped_at DESC LIMIT ?`,
			"%"+escapeLike(strings.ToLower(category))+"%", limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkArticlesSent records that the given articles were included in the user's
// digest, so they won't be re-sent.
func (d *DB) MarkArticlesSent(userID int64, articleIDs []int64) error {
	if len(articleIDs) == 0 {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO user_digests (user_id, article_id) VALUES (?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, aid := range articleIDs {
		if _, err := stmt.Exec(userID, aid); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// --- Cost tracking ---

// LogCost records the cost of one API operation.
func (d *DB) LogCost(service, operation string, units, costUSD float64) error {
	_, err := d.sql.Exec(
		`INSERT INTO api_costs (service, operation, units, cost_usd) VALUES (?, ?, ?, ?)`,
		service, operation, units, costUSD,
	)
	return err
}

// GetCostSummary returns per-service, per-day cost totals, newest day first.
func (d *DB) GetCostSummary() ([]CostRow, error) {
	rows, err := d.sql.Query(`
		SELECT service, SUM(cost_usd) as total, DATE(logged_at) as date
		FROM api_costs
		GROUP BY service, DATE(logged_at)
		ORDER BY date DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostRow
	for rows.Next() {
		var c CostRow
		var svc, date sql.NullString
		var total sql.NullFloat64
		if err := rows.Scan(&svc, &total, &date); err != nil {
			return nil, err
		}
		c.Service = svc.String
		c.Total = total.Float64
		c.Date = date.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// MonthlyCostBreakdown is one service's total for the current month.
type MonthlyCostBreakdown struct {
	Service     string  `json:"service"`
	TotalUSD    float64 `json:"total_usd"`
	APICalls    int64   `json:"api_calls"`
	TotalTokens float64 `json:"total_tokens"`
	Month       string  `json:"month"`
}

// GetMonthlyCosts returns per-service totals for the current month plus the
// grand total across all services.
func (d *DB) GetMonthlyCosts() (grandTotal float64, breakdown []MonthlyCostBreakdown, err error) {
	rows, err := d.sql.Query(`
		SELECT service, SUM(cost_usd), COUNT(*), SUM(units), strftime('%Y-%m', logged_at)
		FROM api_costs
		WHERE strftime('%Y-%m', logged_at) = strftime('%Y-%m', 'now')
		GROUP BY service`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b MonthlyCostBreakdown
		var svc, month sql.NullString
		var total, tokens sql.NullFloat64
		var calls sql.NullInt64
		if err := rows.Scan(&svc, &total, &calls, &tokens, &month); err != nil {
			return 0, nil, err
		}
		b.Service = svc.String
		b.TotalUSD = total.Float64
		b.APICalls = calls.Int64
		b.TotalTokens = tokens.Float64
		b.Month = month.String
		breakdown = append(breakdown, b)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	var gt sql.NullFloat64
	err = d.sql.QueryRow(
		`SELECT SUM(cost_usd) FROM api_costs WHERE strftime('%Y-%m', logged_at) = strftime('%Y-%m', 'now')`,
	).Scan(&gt)
	if err != nil {
		return 0, nil, err
	}
	return gt.Float64, breakdown, nil
}

// GetCostToday returns the total spend logged today.
func (d *DB) GetCostToday() (float64, error) {
	var total sql.NullFloat64
	err := d.sql.QueryRow(
		`SELECT SUM(cost_usd) FROM api_costs WHERE DATE(logged_at) = DATE('now')`,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Float64, nil
}
