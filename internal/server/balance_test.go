package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
)

// balanceServer serves a canned balance payload and records the auth header.
func balanceServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotAuth string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, &gotAuth
}

func TestFetchBalanceVariants(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   float64
		wantEr bool
	}{
		{name: "single currency", status: 200,
			body: `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"12.34"}]}`,
			want: 12.34},
		{name: "multiple currencies summed", status: 200,
			body: `{"balance_infos":[{"currency":"USD","total_balance":"1.50"},{"currency":"CNY","total_balance":"2.50"}]}`,
			want: 4.0},
		{name: "zero balance", status: 200,
			body: `{"balance_infos":[{"currency":"USD","total_balance":"0"}]}`,
			want: 0},
		{name: "junk entry skipped", status: 200,
			body: `{"balance_infos":[{"total_balance":"abc"},{"total_balance":"3"}]}`,
			want: 3},
		{name: "all entries junk", status: 200,
			body: `{"balance_infos":[{"total_balance":"abc"}]}`, wantEr: true},
		{name: "empty list", status: 200, body: `{"balance_infos":[]}`, wantEr: true},
		{name: "not json", status: 200, body: `nope`, wantEr: true},
		{name: "unauthorized", status: 401, body: `{"error":"bad key"}`, wantEr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, gotAuth := balanceServer(t, tc.status, tc.body)
			got, err := fetchBalance(context.Background(), srv.URL, "sk-test")
			if tc.wantEr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("balance = %v, want %v", got, tc.want)
			}
			if *gotAuth != "Bearer sk-test" {
				t.Fatalf("Authorization = %q, want the provider key", *gotAuth)
			}
		})
	}
}

// probeRouter builds a 2-candidate route on the given providers.
func probeRouter(names ...string) *router.Router {
	provs := make([]provider.Provider, 0, len(names))
	cands := make([]router.Candidate, 0, len(names))
	for i, n := range names {
		p := &probeProvider{name: n, priority: i + 1}
		provs = append(provs, p)
		cands = append(cands, router.Candidate{Provider: p, Model: "m"})
	}
	return router.New(provs, []*router.Route{{Model: "auto", Strategy: router.StrategyFillFirst, Candidates: cands}})
}

type probeProvider struct {
	name     string
	priority int
}

func (p *probeProvider) Name() string      { return p.name }
func (p *probeProvider) Priority() int     { return p.priority }
func (p *probeProvider) ModelsURL() string { return "" }
func (p *probeProvider) Models(context.Context) ([]string, error) {
	return nil, nil
}
func (p *probeProvider) ChatComplete(context.Context, *provider.Request) (*http.Response, error) {
	return nil, nil
}
func (p *probeProvider) Embed(context.Context, *provider.Request) (*http.Response, error) {
	return nil, nil
}

// A balance below min_usd marks the provider, quota-aware strategies sink it,
// and a later probe above the threshold clears the mark.
func TestBalanceProbeMarksAndClears(t *testing.T) {
	body := `{"balance_infos":[{"currency":"USD","total_balance":"0.02"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	rt := probeRouter("poor", "rich")
	target := BalanceTarget{Provider: "poor", URL: srv.URL, APIKey: "k", MinUSD: 0.10}

	probeBalanceOnce(context.Background(), rt, target)
	if !rt.Quota().BalanceLow("poor") {
		t.Fatal("balance 0.02 < min 0.10 must mark the provider low")
	}
	// The ledger reports every model of that provider exhausted...
	rem, ratio, known := rt.Quota().Remaining("poor", "m")
	if !known || rem != 0 || ratio != 0 {
		t.Fatalf("Remaining = %d/%v known=%v, want 0/0/true", rem, ratio, known)
	}
	// ...and an untouched provider is unaffected.
	if _, _, known := rt.Quota().Remaining("rich", "m"); known {
		t.Fatal("unmarked provider must stay unknown (fail-open)")
	}
	// fill_first sinks the low-balance candidate behind the healthy one.
	got := rt.OrderCandidates(rt.Resolve("auto"))
	if got[0].Provider.Name() != "rich" {
		t.Fatalf("first candidate = %s, want rich (poor is out of balance)", got[0].Provider.Name())
	}

	// Recovery: the next probe above the threshold clears the mark.
	body = `{"balance_infos":[{"currency":"USD","total_balance":"25.00"}]}`
	probeBalanceOnce(context.Background(), rt, target)
	if rt.Quota().BalanceLow("poor") {
		t.Fatal("balance above min must clear the mark")
	}
	if _, _, known := rt.Quota().Remaining("poor", "m"); known {
		t.Fatal("cleared provider must report unknown again")
	}
}

// A failing probe (network, 401, junk) changes nothing — never fail closed.
func TestBalanceProbeErrorIsNoop(t *testing.T) {
	rt := probeRouter("p")
	// Unreachable endpoint.
	probeBalanceOnce(context.Background(), rt, BalanceTarget{Provider: "p", URL: "http://127.0.0.1:1", MinUSD: 1})
	if rt.Quota().BalanceLow("p") {
		t.Fatal("an unreachable probe must not mark the provider low")
	}
	// 401.
	srv, _ := balanceServer(t, 401, `{"error":"bad key"}`)
	probeBalanceOnce(context.Background(), rt, BalanceTarget{Provider: "p", URL: srv.URL, MinUSD: 1})
	if rt.Quota().BalanceLow("p") {
		t.Fatal("a 401 probe must not mark the provider low")
	}
	// A probe failure must also not CLEAR an existing mark.
	rt.Quota().SetBalanceLow("p", true)
	probeBalanceOnce(context.Background(), rt, BalanceTarget{Provider: "p", URL: srv.URL, MinUSD: 1})
	if !rt.Quota().BalanceLow("p") {
		t.Fatal("a failed probe must leave the existing mark alone")
	}
}

// With no balance_probe configured, no goroutine runs and nothing is marked.
func TestBalanceProbeDisabledByDefault(t *testing.T) {
	rt := probeRouter("p")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	RunBalanceProbes(ctx, rt, nil)
	time.Sleep(20 * time.Millisecond)
	if rt.Quota().BalanceLow("p") {
		t.Fatal("no targets must mean no marks")
	}
	if _, _, known := rt.Quota().Remaining("p", "m"); known {
		t.Fatal("quota must stay unknown when probing is off")
	}
}
