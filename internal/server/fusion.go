package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
)

// fusionResult is one arm's outcome in a fusion race.
type fusionResult struct {
	cand     router.Candidate
	resp     *http.Response // deterministic response (2xx/4xx), nil otherwise
	failResp *http.Response // buffered retryable upstream error
	err      error          // transport error
}

// fusionRun races the first 2 candidates concurrently. First 200 wins; when
// both return 200 the cheaper one (by price table, unknown = tie) wins and
// the loser's body is discarded. Both-fail mirrors the sequential all-fail
// path. Chat-only: callers gate on !stream.
func (s *srv) fusionRun(ctx context.Context, hdr http.Header, body []byte, pair []router.Candidate) (cand router.Candidate, resp, lastFailResp *http.Response, lastErr error, attempts int) {
	attempts = len(pair)
	results := make([]fusionResult, len(pair))
	var wg sync.WaitGroup
	for i, c := range pair {
		wg.Add(1)
		go func(i int, c router.Candidate) {
			defer wg.Done()
			attemptStart := time.Now()
			req := &provider.Request{Model: c.Model, Body: body, Header: hdr}
			s.router.IncInflight(c.Provider.Name())
			att, err := c.Provider.ChatComplete(ctx, req)
			if err == nil {
				s.observeUpstreamQuota(c.Provider.Name(), c.Model, att.Header)
			}
			switch {
			case err != nil:
				s.router.DecInflight(c.Provider.Name())
				s.router.RecordResult(c.Provider.Name(), time.Since(attemptStart), false)
				results[i].err = err
			case s.retryableStatus(att.StatusCode):
				errBody, _ := readClose(att)
				s.router.DecInflight(c.Provider.Name())
				s.router.RecordResult(c.Provider.Name(), time.Since(attemptStart), false)
				if att.StatusCode == http.StatusTooManyRequests {
					s.router.LockModel(c.Provider.Name(), c.Model, 30*time.Second)
					if d := parseRetryAfter(att.Header.Get("Retry-After")); d > 0 {
						s.router.OpenCircuitFor(c.Provider.Name(), d)
					}
				}
				results[i].failResp = &http.Response{
					StatusCode: att.StatusCode, Header: att.Header,
					Body: errBody,
				}
			default:
				if att.StatusCode == http.StatusNotFound {
					s.router.LockModel(c.Provider.Name(), c.Model, 30*time.Second)
				}
				s.router.RecordResult(c.Provider.Name(), time.Since(attemptStart), true)
				// Inflight clears when the body closes (winner: after relay;
				// loser: discarded below).
				att.Body = &inflightBody{ReadCloser: att.Body, done: func() {
					s.router.DecInflight(c.Provider.Name())
				}}
				results[i].resp = att
			}
			results[i].cand = c
		}(i, c)
	}
	wg.Wait()

	// Collect successes; prefer 200, then lower estimated cost, then arrival
	// order (index order here — bodies are buffered anyway).
	type hit struct {
		idx  int
		resp *http.Response
	}
	var hits []hit
	for i, r := range results {
		if r.resp != nil && r.resp.StatusCode == http.StatusOK {
			hits = append(hits, hit{i, r.resp})
		}
	}
	pick := -1
	if len(hits) == 1 {
		pick = hits[0].idx
	} else if len(hits) == 2 {
		pick = hits[0].idx
		if s.fusionCost(pair[hits[1].idx].Model) < s.fusionCost(pair[hits[0].idx].Model) {
			pick = hits[1].idx
		}
	}
	if pick >= 0 {
		for i := range results {
			if i == pick {
				continue
			}
			if results[i].resp != nil {
				results[i].resp.Body.Close() // loser discarded
			}
		}
		return pair[pick], results[pick].resp, nil, nil, attempts
	}
	// No 200: a deterministic non-200 (4xx/501) relays as-is, mirroring the
	// sequential path's no-failover rule; prefer the first such response.
	for i := range results {
		if results[i].resp != nil {
			for j := range results {
				if j != i && results[j].resp != nil {
					results[j].resp.Body.Close()
				}
			}
			return results[i].cand, results[i].resp, nil, nil, attempts
		}
	}
	// All failed: relay last retryable response as-is, else 502 upstream_error.
	cand = pair[len(pair)-1]
	var lastFail *http.Response
	for _, r := range results {
		if r.failResp != nil {
			if lastFail != nil {
				lastFail.Body.Close()
			}
			lastFail = r.failResp
		}
		if r.err != nil {
			lastErr = r.err
		}
	}
	if lastFail != nil {
		return cand, nil, lastFail, nil, attempts
	}
	if lastErr != nil {
		return cand, nil, nil, lastErr, attempts
	}
	return cand, nil, nil, errors.New("fusion: no result"), attempts
}

// readClose buffers up to 64KB of an upstream error body and closes it.
func readClose(resp *http.Response) (io.ReadCloser, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return io.NopCloser(bytes.NewReader(b)), err
}

// fusionCost is prompt+completion price for tie-breaking; unknown = +Inf-ish.
func (s *srv) fusionCost(model string) float64 {
	if p, ok := s.prices[model]; ok {
		return p.PromptPer1M + p.CompletionPer1M
	}
	return 1e18
}
