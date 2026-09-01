// Package auth manages virtual API keys stored in the shared SQLite DB.
package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Key is a virtual API key for gateway clients.
type Key struct {
	ID            int64      `json:"id"`
	Key           string     `json:"key"`
	Name          string     `json:"name"`
	RPM           int        `json:"rpm"`          // requests/min; 0 = unlimited
	TPM           int        `json:"tpm"`          // tokens/min; 0 = unlimited
	ModelRPM      int        `json:"model_rpm"`    // per-(key,model) requests/min; 0 = use RPM
	LimitByHeader string     `json:"limit_by_header"` // when set, rate-limit identity = this header's value
	DailyQuota    int64      `json:"daily_quota"`     // max requests per UTC day; 0 = unlimited
	DailyUsed     int64      `json:"daily_used"`      // requests used in the current UTC day
	QuotaTokens   int64      `json:"quota_tokens"` // lifetime cap; 0 = unlimited
	SpentTokens   int64      `json:"spent_tokens"`
	BudgetUSD     float64    `json:"budget_usd"` // lifetime USD cap; 0 = unlimited
	SpentUSD      float64    `json:"spent_usd"`
	AllowedModels []string   `json:"allowed_models"` // empty = all
	Groups        []string   `json:"groups"`         // empty = all candidate groups usable
	ExpiresAt     *time.Time `json:"expires_at"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
}

// GenerateKey returns a random key string. With a non-empty name the prefix
// is "gw-<slug>-" (slug from name) + 24 hex chars; otherwise the back-compat
// shape "gw-" + 32 hex chars.
func GenerateKey(name string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	if slug := slugify(name); slug != "" {
		return "gw-" + slug + "-" + hex.EncodeToString(b[:12])
	}
	return "gw-" + hex.EncodeToString(b[:])
}

// slugify lowercases, maps every non-alnum run to "-", trims dashes, and caps
// at 20 chars. "" for names with no alnum characters.
func slugify(s string) string {
	var out []byte
	dash := false
	for _, r := range strings.ToLower(s) {
		isAlnum := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		switch {
		case isAlnum:
			out = append(out, byte(r))
			dash = false
		case !dash && len(out) > 0:
			out = append(out, '-')
			dash = true
		}
		if len(out) >= 20 {
			break
		}
	}
	return strings.TrimRight(string(out), "-")
}

// Store persists keys in SQLite. Share the *sql.DB with usage.Logger
// via usage.OpenDB.
type Store struct {
	db *sql.DB
}

// NewStore creates the api_keys table on the given DB.
func NewStore(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS api_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT UNIQUE,
		name TEXT,
		rpm INTEGER,
		tpm INTEGER,
		quota_tokens INTEGER,
		spent_tokens INTEGER DEFAULT 0,
		allowed_models TEXT,
		expires_at TEXT,
		enabled INTEGER,
		created_at TEXT
	)`); err != nil {
		return nil, fmt.Errorf("create api_keys table: %w", err)
	}
	// Batch 4/5/6 columns; add only when missing (existing DBs).
	for _, col := range []string{"budget_usd REAL DEFAULT 0", "spent_usd REAL DEFAULT 0", `groups TEXT DEFAULT ''`, "model_rpm INTEGER DEFAULT 0", "limit_by_header TEXT DEFAULT ''", "daily_quota INTEGER DEFAULT 0", "daily_used INTEGER DEFAULT 0", "daily_day TEXT DEFAULT ''"} {
		name := strings.SplitN(col, " ", 2)[0]
		if !hasColumn(db, "api_keys", name) {
			if _, err := db.Exec(`ALTER TABLE api_keys ADD COLUMN ` + col); err != nil {
				return nil, fmt.Errorf("migrate api_keys: %w", err)
			}
		}
	}
	return &Store{db: db}, nil
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

// Create inserts a key, generating the key string from the name when empty.
func (s *Store) Create(k Key) (Key, error) {
	if k.Key == "" {
		k.Key = GenerateKey(k.Name)
	}
	models, err := json.Marshal(k.AllowedModels)
	if err != nil {
		return Key{}, err
	}
	groups, err := json.Marshal(k.Groups)
	if err != nil {
		return Key{}, err
	}
	var expires any
	if k.ExpiresAt != nil {
		expires = k.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	k.CreatedAt = time.Now().UTC()
	res, err := s.db.Exec(`INSERT INTO api_keys
		(key, name, rpm, tpm, model_rpm, limit_by_header, daily_quota, quota_tokens, spent_tokens, budget_usd, spent_usd, allowed_models, groups, expires_at, enabled, created_at)
		VALUES (?,?,?,?,?,?,?,?,0,?,0,?,?,?,?,?)`,
		k.Key, k.Name, k.RPM, k.TPM, k.ModelRPM, k.LimitByHeader, k.DailyQuota, k.QuotaTokens, k.BudgetUSD, string(models), string(groups), expires, boolInt(k.Enabled),
		k.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Key{}, err
	}
	k.ID, _ = res.LastInsertId()
	return k, nil
}

// GetByKey returns the key row or (nil, nil) when unknown.
func (s *Store) GetByKey(key string) (*Key, error) {
	rows, err := s.db.Query(`SELECT id, key, name, rpm, tpm, COALESCE(model_rpm,0), COALESCE(limit_by_header,''),
		COALESCE(daily_quota,0), COALESCE(daily_used,0), COALESCE(daily_day,''), quota_tokens, spent_tokens,
		COALESCE(budget_usd,0), COALESCE(spent_usd,0),
		allowed_models, COALESCE(groups,''), expires_at, enabled, created_at FROM api_keys WHERE key = ?`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	k, err := scanKey(rows)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// List returns all keys, newest first.
func (s *Store) List() ([]Key, error) {
	rows, err := s.db.Query(`SELECT id, key, name, rpm, tpm, COALESCE(model_rpm,0), COALESCE(limit_by_header,''),
		COALESCE(daily_quota,0), COALESCE(daily_used,0), COALESCE(daily_day,''), quota_tokens, spent_tokens,
		COALESCE(budget_usd,0), COALESCE(spent_usd,0),
		allowed_models, COALESCE(groups,''), expires_at, enabled, created_at FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func scanKey(rows *sql.Rows) (Key, error) {
	var k Key
	var models, groups, created, dailyDay string
	var expires sql.NullString
	var enabled int
	if err := rows.Scan(&k.ID, &k.Key, &k.Name, &k.RPM, &k.TPM, &k.ModelRPM, &k.LimitByHeader,
		&k.DailyQuota, &k.DailyUsed, &dailyDay, &k.QuotaTokens,
		&k.SpentTokens, &k.BudgetUSD, &k.SpentUSD, &models, &groups, &expires, &enabled, &created); err != nil {
		return Key{}, err
	}
	// Reset the daily counter when the UTC day has rolled over.
	if today := time.Now().UTC().Format("2006-01-02"); dailyDay != today {
		k.DailyUsed = 0
	}
	_ = json.Unmarshal([]byte(models), &k.AllowedModels)
	_ = json.Unmarshal([]byte(groups), &k.Groups)
	k.Enabled = enabled != 0
	k.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if expires.Valid {
		if t, err := time.Parse(time.RFC3339Nano, expires.String); err == nil {
			k.ExpiresAt = &t
		}
	}
	return k, nil
}

// SetEnabled flips the enabled flag.
func (s *Store) SetEnabled(id int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE api_keys SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
	return err
}

// Delete removes a key.
func (s *Store) Delete(id int64) error {
	_, err := s.db.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

// SpendTokens adds n to the key's spent_tokens counter.
func (s *Store) SpendTokens(id int64, n int) error {
	_, err := s.db.Exec(`UPDATE api_keys SET spent_tokens = spent_tokens + ? WHERE id = ?`, n, id)
	return err
}

// SpendUSD adds amount to the key's spent_usd counter.
func (s *Store) SpendUSD(id int64, amount float64) error {
	_, err := s.db.Exec(`UPDATE api_keys SET spent_usd = spent_usd + ? WHERE id = ?`, amount, id)
	return err
}

// IncrDaily atomically increments the daily request counter, resetting it
// first when the stored day differs from today (UTC).
func (s *Store) IncrDaily(id int64) error {
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(`UPDATE api_keys SET
		daily_used = CASE WHEN daily_day = ? THEN daily_used + 1 ELSE 1 END,
		daily_day = ?
		WHERE id = ?`, today, today, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
