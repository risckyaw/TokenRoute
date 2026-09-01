// Package auth manages virtual API keys stored in the shared SQLite DB.
package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Key is a virtual API key for gateway clients.
type Key struct {
	ID            int64      `json:"id"`
	Key           string     `json:"key"`
	Name          string     `json:"name"`
	RPM           int        `json:"rpm"`          // requests/min; 0 = unlimited
	TPM           int        `json:"tpm"`          // tokens/min; 0 = unlimited
	QuotaTokens   int64      `json:"quota_tokens"` // lifetime cap; 0 = unlimited
	SpentTokens   int64      `json:"spent_tokens"`
	AllowedModels []string   `json:"allowed_models"` // empty = all
	ExpiresAt     *time.Time `json:"expires_at"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
}

// GenerateKey returns "gw-" + 32 hex chars from crypto/rand.
func GenerateKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "gw-" + hex.EncodeToString(b[:])
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
	return &Store{db: db}, nil
}

// Create inserts a key, generating the key string when empty.
func (s *Store) Create(k Key) (Key, error) {
	if k.Key == "" {
		k.Key = GenerateKey()
	}
	models, err := json.Marshal(k.AllowedModels)
	if err != nil {
		return Key{}, err
	}
	var expires any
	if k.ExpiresAt != nil {
		expires = k.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	k.CreatedAt = time.Now().UTC()
	res, err := s.db.Exec(`INSERT INTO api_keys
		(key, name, rpm, tpm, quota_tokens, spent_tokens, allowed_models, expires_at, enabled, created_at)
		VALUES (?,?,?,?,?,0,?,?,?,?)`,
		k.Key, k.Name, k.RPM, k.TPM, k.QuotaTokens, string(models), expires, boolInt(k.Enabled),
		k.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Key{}, err
	}
	k.ID, _ = res.LastInsertId()
	return k, nil
}

// GetByKey returns the key row or (nil, nil) when unknown.
func (s *Store) GetByKey(key string) (*Key, error) {
	rows, err := s.db.Query(`SELECT id, key, name, rpm, tpm, quota_tokens, spent_tokens,
		allowed_models, expires_at, enabled, created_at FROM api_keys WHERE key = ?`, key)
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
	rows, err := s.db.Query(`SELECT id, key, name, rpm, tpm, quota_tokens, spent_tokens,
		allowed_models, expires_at, enabled, created_at FROM api_keys ORDER BY id DESC`)
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
	var models, created string
	var expires sql.NullString
	var enabled int
	if err := rows.Scan(&k.ID, &k.Key, &k.Name, &k.RPM, &k.TPM, &k.QuotaTokens,
		&k.SpentTokens, &models, &expires, &enabled, &created); err != nil {
		return Key{}, err
	}
	_ = json.Unmarshal([]byte(models), &k.AllowedModels)
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
