// Package usage records per-request token usage and cost in SQLite.
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // driver name "sqlite"
)

// Price is the per-model pricing in USD per 1M tokens.
type Price struct {
	PromptPer1M     float64 `yaml:"prompt_per_1m"`
	CompletionPer1M float64 `yaml:"completion_per_1m"`
	EmbedPer1M      float64 `yaml:"embed_per_1m"`   // embeddings; 0 = fall back to PromptPer1M
	ContextTokens   int     `yaml:"context_tokens"` // model context window; 0 = no guard
	// Expr is an optional expression (new-api billingexpr port) evaluated per
	// request with token-detail variables (p,c,len,cr,cc,cc1h,img,ai,ao).
	// When non-empty it wins over the flat fields entirely for chat cost.
	Expr string `yaml:"expr"`
}

// Entry is one logged request.
type Entry struct {
	ID               int64     `json:"id,omitempty"`
	RequestID        string    `json:"request_id"`
	TS               time.Time `json:"ts"`
	KeyID            int64     `json:"key_id"`
	KeyName          string    `json:"key_name"`
	VirtualModel     string    `json:"virtual_model"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	Stream           bool      `json:"stream"`
	Status           int       `json:"status"`
	LatencyMs        int64     `json:"latency_ms"`
	CostUSD          *float64  `json:"cost_usd"`
	BudgetExceeded   bool      `json:"budget_exceeded"`
	Cached           bool      `json:"cached"`
	// Multiplier is the route cost multiplier applied to CostUSD (default 1).
	Multiplier float64 `json:"multiplier"`
	// PriceTier is the tier name recorded by a pricing expression's tier()
	// call ("" when flat pricing or the expression never calls tier).
	PriceTier string `json:"price_tier,omitempty"`
	// Token details for expression pricing (not logged separately).
	CacheReadTokens   int `json:"-"`
	CacheCreateTokens int `json:"-"`
	ImageTokens       int `json:"-"`
	AudioInTokens     int `json:"-"`
	AudioOutTokens    int `json:"-"`
}

// KeyAggregate is the per-key usage rollup for the admin API.
type KeyAggregate struct {
	KeyID      int64   `json:"key_id"`
	KeyName    string  `json:"key_name"`
	Requests   int     `json:"requests"`
	TotalToken int     `json:"total_tokens"`
	CostUSD    float64 `json:"cost_usd"`
}

// Cost computes USD cost for the given token counts; nil when price unknown.
func Cost(promptTokens, completionTokens int, p *Price) *float64 {
	if p == nil {
		return nil
	}
	c := float64(promptTokens)/1e6*p.PromptPer1M + float64(completionTokens)/1e6*p.CompletionPer1M
	return &c
}

// EmbedCost computes USD cost for an embeddings request; nil when price
// unknown. Falls back to PromptPer1M when EmbedPer1M is unset.
func EmbedCost(promptTokens int, p *Price) *float64 {
	if p == nil {
		return nil
	}
	per1m := p.EmbedPer1M
	if per1m == 0 {
		per1m = p.PromptPer1M
	}
	c := float64(promptTokens) / 1e6 * per1m
	return &c
}

type Logger struct {
	db *sql.DB
}

// OpenDB opens (creating if needed) the SQLite DB at path, creating parent
// dirs, and creates/migrates the usage_logs table. One *sql.DB is shared by
// usage.Logger and auth.Store.
func OpenDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create usage db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open usage db: %w", err)
	}
	// Concurrent writers (fusion panel calls log a row each, in parallel) hit
	// SQLITE_BUSY and silently lose usage rows. One connection serializes them
	// through the pool instead; WAL (persisted in the file) keeps readers from
	// blocking, and busy_timeout covers the other handle sharing this DB.
	// ponytail: a single connection means a slow query delays writes. Every
	// query here is a bounded LIMIT/aggregate, so that is fine; move to a
	// per-connection DSN pragma + a write mutex if reads ever get heavy.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("usage db %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS usage_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT,
		ts TEXT,
		virtual_model TEXT,
		provider TEXT,
		model TEXT,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		total_tokens INTEGER,
		stream INTEGER,
		status INTEGER,
		latency_ms INTEGER,
		cost_usd REAL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create usage table: %w", err)
	}
	// Phase 4 columns; ALTER fails on existing DBs that have them, so add
	// only when missing (checked via pragma).
	for _, col := range []string{"key_id INTEGER", "key_name TEXT", "budget_exceeded INTEGER", "cached INTEGER", "multiplier REAL DEFAULT 1", "price_tier TEXT"} {
		name := strings.SplitN(col, " ", 2)[0]
		if !hasColumn(db, "usage_logs", name) {
			if _, err := db.Exec(`ALTER TABLE usage_logs ADD COLUMN ` + col); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate usage_logs: %w", err)
			}
		}
	}
	return db, nil
}

func hasColumn(db *sql.DB, table, col string) bool {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil && n == col {
			return true
		}
	}
	return false
}

// NewLogger wraps a shared DB handle.
func NewLogger(db *sql.DB) *Logger { return &Logger{db: db} }

// Open opens (creating if needed) the SQLite DB at path, creating parent dirs.
func Open(path string) (*Logger, error) {
	db, err := OpenDB(path)
	if err != nil {
		return nil, err
	}
	return &Logger{db: db}, nil
}

// Log inserts one entry. Called synchronously after the response relay ends.
func (l *Logger) Log(ctx context.Context, e Entry) error {
	var cost any
	if e.CostUSD != nil {
		cost = *e.CostUSD
	}
	mult := e.Multiplier
	if mult == 0 {
		mult = 1
	}
	var tier any
	if e.PriceTier != "" {
		tier = e.PriceTier
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO usage_logs
		(request_id, ts, key_id, key_name, virtual_model, provider, model, prompt_tokens,
		 completion_tokens, total_tokens, stream, status, latency_ms, cost_usd, budget_exceeded, cached, multiplier, price_tier)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.RequestID, e.TS.UTC().Format(time.RFC3339Nano), e.KeyID, e.KeyName,
		e.VirtualModel, e.Provider, e.Model,
		e.PromptTokens, e.CompletionTokens, e.TotalTokens, boolInt(e.Stream),
		e.Status, e.LatencyMs, cost, boolInt(e.BudgetExceeded), boolInt(e.Cached), mult, tier)
	return err
}

// QueryRecent returns the newest entries, up to limit.
func (l *Logger) QueryRecent(limit int) ([]Entry, error) {
	rows, err := l.db.Query(`SELECT id, request_id, ts, key_id, key_name, virtual_model, provider, model,
		prompt_tokens, completion_tokens, total_tokens, stream, status, latency_ms, cost_usd,
		COALESCE(budget_exceeded,0), COALESCE(cached,0), COALESCE(multiplier,1), COALESCE(price_tier,'')
		FROM usage_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var ts, keyName string
		var stream, budget, cached int
		var cost sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.RequestID, &ts, &e.KeyID, &keyName, &e.VirtualModel, &e.Provider, &e.Model,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &stream,
			&e.Status, &e.LatencyMs, &cost, &budget, &cached, &e.Multiplier, &e.PriceTier); err != nil {
			return nil, err
		}
		e.KeyName = keyName
		e.TS, _ = time.Parse(time.RFC3339Nano, ts)
		e.Stream = stream != 0
		e.BudgetExceeded = budget != 0
		e.Cached = cached != 0
		if cost.Valid {
			c := cost.Float64
			e.CostUSD = &c
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExportRows streams rows in [from, to] to fn one at a time (no buffering).
func (l *Logger) ExportRows(from, to time.Time, fn func(Entry) error) error {
	rows, err := l.db.Query(`SELECT id, request_id, ts, key_id, key_name, virtual_model, provider, model,
		prompt_tokens, completion_tokens, total_tokens, stream, status, latency_ms, cost_usd,
		COALESCE(budget_exceeded,0), COALESCE(cached,0), COALESCE(multiplier,1), COALESCE(price_tier,'')
		FROM usage_logs WHERE ts >= ? AND ts <= ? ORDER BY id`,
		from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var ts, keyName string
		var stream, budget, cached int
		var cost sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.RequestID, &ts, &e.KeyID, &keyName, &e.VirtualModel, &e.Provider, &e.Model,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &stream,
			&e.Status, &e.LatencyMs, &cost, &budget, &cached, &e.Multiplier, &e.PriceTier); err != nil {
			return err
		}
		e.KeyName = keyName
		e.TS, _ = time.Parse(time.RFC3339Nano, ts)
		e.Stream = stream != 0
		e.BudgetExceeded = budget != 0
		e.Cached = cached != 0
		if cost.Valid {
			c := cost.Float64
			e.CostUSD = &c
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// AggregateByKey rolls up requests/tokens/cost per API key (key_id 0 =
// requests logged without a key, e.g. pre-Phase-4 rows).
func (l *Logger) AggregateByKey() ([]KeyAggregate, error) {
	rows, err := l.db.Query(`SELECT key_id, COALESCE(key_name,''), COUNT(*),
		COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0)
		FROM usage_logs GROUP BY key_id, key_name ORDER BY key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyAggregate{}
	for rows.Next() {
		var a KeyAggregate
		if err := rows.Scan(&a.KeyID, &a.KeyName, &a.Requests, &a.TotalToken, &a.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (l *Logger) Close() error { return l.db.Close() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
