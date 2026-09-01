package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// slowProvider waits for ctx done or 200ms.
type slowProvider struct {
	fakeProvider
	lastHeader http.Header
}

func (p *slowProvider) Name() string  { return "slow" }
func (p *slowProvider) Priority() int { return 1 }
func (p *slowProvider) ChatComplete(ctx context.Context, req *provider.Request) (*http.Response, error) {
	p.lastHeader = req.Header
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(200 * time.Millisecond):
		return p.fakeProvider.ChatComplete(ctx, req)
	}
}

func TestTimeoutOverride(t *testing.T) {
	sp := &slowProvider{fakeProvider: fakeProvider{nonStream: true, body: `{"choices":[]}`}}
	rt := router.New([]provider.Provider{sp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: sp, Model: "up-model"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := NewWithOptions(Options{Router: rt, Usage: ul})

	// 50ms cap vs 200ms upstream -> 502 (transport error, context deadline).
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
	req.Header.Set("X-Timeout-Ms", "50")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body)
	}

	// Header absent -> 200.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}

	// Header not forwarded upstream.
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
	req.Header.Set("X-Timeout-Ms", "5000")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if sp.lastHeader.Get("X-Timeout-Ms") != "" {
		t.Fatal("X-Timeout-Ms leaked upstream")
	}
}
