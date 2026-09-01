package auth

import (
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestDailyQuotaIncrementAndReset(t *testing.T) {
	db, err := usage.OpenDB(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	k, err := st.Create(Key{Name: "d", RPM: 0, DailyQuota: 5, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	for i := int64(1); i <= 3; i++ {
		if err := st.IncrDaily(k.ID); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetByKey(k.Key)
		if err != nil {
			t.Fatal(err)
		}
		if got.DailyUsed != i {
			t.Fatalf("DailyUsed = %d, want %d", got.DailyUsed, i)
		}
	}

	// Simulate a stored day from the past: counter resets on next read.
	if _, err := db.Exec(`UPDATE api_keys SET daily_day = '2000-01-01', daily_used = 99 WHERE id = ?`, k.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetByKey(k.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyUsed != 0 {
		t.Errorf("stale day should reset DailyUsed, got %d", got.DailyUsed)
	}

	// IncrDaily after stale day starts from 1, not 100.
	if err := st.IncrDaily(k.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetByKey(k.Key)
	if got.DailyUsed != 1 {
		t.Errorf("IncrDaily after rollover = %d, want 1", got.DailyUsed)
	}
}
