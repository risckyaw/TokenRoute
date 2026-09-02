package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/router"
)

// BalanceTarget is one provider's balance probe (9router usage/deepseek.js):
// a DeepSeek-style GET returning balance_infos[].total_balance. Any provider
// exposing that shape works — the URL is explicit config, not hardcoded.
type BalanceTarget struct {
	Provider string // provider name (the router key)
	URL      string
	APIKey   string // sent as "Authorization: Bearer ..."; may be empty
	Interval time.Duration
	MinUSD   float64
}

// balanceClient bounds each probe; balance endpoints are small and fast.
var balanceClient = &http.Client{Timeout: 15 * time.Second}

// RunBalanceProbes polls each target on its interval until ctx is cancelled.
// A balance below MinUSD marks the provider low in the quota ledger, which
// quota-aware strategies (reset_aware, fill_first, auto) read as exhausted, so
// traffic drains off BEFORE an empty account starts answering 429s. A probe
// above the threshold clears the mark; probe failures (network, 401, junk
// payload) are logged and change nothing — never fail closed on a broken probe.
func RunBalanceProbes(ctx context.Context, rt *router.Router, targets []BalanceTarget) {
	for _, t := range targets {
		t := t
		if t.Interval <= 0 {
			t.Interval = 5 * time.Minute
		}
		go func() {
			ticker := time.NewTicker(t.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					probeBalanceOnce(ctx, rt, t)
				}
			}
		}()
	}
}

// probeBalanceOnce performs one balance probe and updates the ledger mark.
func probeBalanceOnce(ctx context.Context, rt *router.Router, t BalanceTarget) {
	total, err := fetchBalance(ctx, t.URL, t.APIKey)
	if err != nil {
		slog.Warn("balance probe", "provider", t.Provider, "err", err)
		return
	}
	low := total < t.MinUSD
	was := rt.Quota().BalanceLow(t.Provider)
	rt.Quota().SetBalanceLow(t.Provider, low)
	switch {
	case low && !was:
		slog.Warn("provider balance low", "provider", t.Provider, "balance_usd", total, "min_usd", t.MinUSD)
	case !low && was:
		slog.Info("provider balance recovered", "provider", t.Provider, "balance_usd", total)
	}
}

// balanceResponse is the DeepSeek /user/balance shape (trimmed).
type balanceResponse struct {
	BalanceInfos []struct {
		Currency     string `json:"currency"`
		TotalBalance string `json:"total_balance"`
	} `json:"balance_infos"`
}

// fetchBalance GETs the balance endpoint and sums total_balance across the
// returned currency entries.
//
// Summing (rather than taking the first) is deliberate: a DeepSeek account with
// separate CNY and USD entries has spendable credit in both, and the threshold
// asks "is there money left at all". The sum is NOT currency-converted — it is
// a floor check against min_usd, not an accounting figure. Single-currency
// accounts (the common case) are unaffected either way.
func fetchBalance(ctx context.Context, url, apiKey string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := balanceClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, &probeError{status: resp.StatusCode}
	}
	var parsed balanceResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, err
	}
	if len(parsed.BalanceInfos) == 0 {
		return 0, &probeError{msg: "no balance_infos in response"}
	}
	total := 0.0
	seen := false
	for _, bi := range parsed.BalanceInfos {
		v, err := strconv.ParseFloat(strings.TrimSpace(bi.TotalBalance), 64)
		if err != nil {
			continue // junk entry: skip, don't fail the whole probe
		}
		total += v
		seen = true
	}
	if !seen {
		return 0, &probeError{msg: "no parseable total_balance"}
	}
	return total, nil
}

// probeError reports a non-200 probe or an unusable payload.
type probeError struct {
	status int
	msg    string
}

func (e *probeError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return "balance endpoint status " + strconv.Itoa(e.status)
}
