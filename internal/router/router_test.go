package router

import (
	"context"
	"net/http"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

type fakeProvider struct {
	name     string
	priority int
}

func (f *fakeProvider) Name() string      { return f.name }
func (f *fakeProvider) Priority() int     { return f.priority }
func (f *fakeProvider) ModelsURL() string { return "" }
func (f *fakeProvider) Models(context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeProvider) ChatComplete(context.Context, *provider.Request) (*http.Response, error) {
	return nil, nil
}
func (f *fakeProvider) Embed(context.Context, *provider.Request) (*http.Response, error) {
	return nil, nil
}

func setup() (*Router, *fakeProvider, *fakeProvider, *fakeProvider) {
	hi := &fakeProvider{name: "hi", priority: 1}
	mid := &fakeProvider{name: "mid", priority: 5}
	lo := &fakeProvider{name: "lo", priority: 10}
	provs := []provider.Provider{lo, hi, mid} // intentionally unsorted
	routes := []*Route{
		{
			Model: "auto",
			Candidates: []Candidate{
				{Provider: lo, Model: "lo-model"},
				{Provider: hi, Model: "hi-model"},
				{Provider: mid, Model: "mid-model"},
			},
		},
	}
	return New(provs, routes), hi, mid, lo
}

func TestResolve(t *testing.T) {
	rt, hi, mid, lo := setup()

	tests := []struct {
		name      string
		model     string
		wantNames []string // provider names in priority order; nil = no route
	}{
		{name: "route match sorts by priority", model: "auto", wantNames: []string{"hi", "mid", "lo"}},
		{name: "unknown model returns nil", model: "nonexistent", wantNames: nil},
		{name: "empty model returns nil", model: "", wantNames: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route := rt.Resolve(tc.model)
			if tc.wantNames == nil {
				if route != nil {
					t.Fatalf("got %v, want nil", route)
				}
				return
			}
			cands := rt.OrderCandidates(route)
			if len(cands) != len(tc.wantNames) {
				t.Fatalf("got %d candidates, want %d", len(cands), len(tc.wantNames))
			}
			for i, want := range tc.wantNames {
				if cands[i].Provider.Name() != want {
					t.Errorf("candidate[%d] = %s, want %s", i, cands[i].Provider.Name(), want)
				}
			}
			// candidate model mapping preserved
			if cands[0].Model != "hi-model" {
				t.Errorf("first candidate model = %s, want hi-model", cands[0].Model)
			}
		})
	}
	_ = hi
	_ = mid
	_ = lo
}

func TestProvidersSortedByPriority(t *testing.T) {
	rt, _, _, _ := setup()
	provs := rt.Providers()
	want := []string{"hi", "mid", "lo"}
	if len(provs) != len(want) {
		t.Fatalf("got %d providers, want %d", len(provs), len(want))
	}
	for i, w := range want {
		if provs[i].Name() != w {
			t.Errorf("providers[%d] = %s, want %s", i, provs[i].Name(), w)
		}
	}
}

func TestRouteModels(t *testing.T) {
	rt, _, _, _ := setup()
	ms := rt.RouteModels()
	if len(ms) != 1 || ms[0] != "auto" {
		t.Fatalf("RouteModels = %v, want [auto]", ms)
	}
}
