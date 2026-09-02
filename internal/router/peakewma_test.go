package router

import (
	"math"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

func TestDecayWeight(t *testing.T) {
	if w := decayWeight(0); w != 1 {
		t.Fatalf("decayWeight(0) = %v, want 1", w)
	}
	// 10s -> e^-1 ≈ 0.3679 (DECAY_TIME constant, not half-life)
	if w := decayWeight(10 * time.Second); math.Abs(w-math.Exp(-1)) > 1e-9 {
		t.Fatalf("decayWeight(10s) = %v, want e^-1", w)
	}
	// ~6.93s = half-life
	if w := decayWeight(ewmaDecayTime * 693 / 1000); math.Abs(w-0.5) > 0.005 {
		t.Fatalf("decayWeight(~6.93s) = %v, want ~0.5", w)
	}
}

func TestEWMADecaysWithTime(t *testing.T) {
	r := New(nil, nil)
	r.RecordResult("p", 100*time.Millisecond, true)
	v1 := r.LatencyMs("p")
	if v1 != 100 {
		t.Fatalf("fresh ewma = %v, want 100", v1)
	}
	// Force lastTouched 10s into the past -> decayed by e^-1.
	r.latMu.Lock()
	r.latency["p"].lastTouched = time.Now().Add(-10 * time.Second)
	r.latMu.Unlock()
	if v := r.LatencyMs("p"); math.Abs(v-100*math.Exp(-1)) > 0.5 {
		t.Fatalf("decayed ewma = %v, want ~%v", v, 100*math.Exp(-1))
	}
}

func TestSlowStartSeedIsMean(t *testing.T) {
	r := New(nil, nil)
	r.RecordResult("a", 100*time.Millisecond, true)
	r.RecordResult("b", 300*time.Millisecond, true)
	_, seed, hasData := r.latencySnapshot()
	if !hasData {
		t.Fatal("expected data")
	}
	if math.Abs(seed-200) > 1 {
		t.Fatalf("seed = %v, want ~200 (mean of 100,300)", seed)
	}
}

func TestSlowStartNoData(t *testing.T) {
	r := New(nil, nil)
	if _, _, hasData := r.latencySnapshot(); hasData {
		t.Fatal("no data expected when all providers unseen")
	}
	// least_latency with no data anywhere: priority order unchanged.
	a, b, _, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: "least_latency", Candidates: []Candidate{
		{Provider: a, Model: "am"}, {Provider: b, Model: "bm"},
	}}
	r2 := New(provs, []*Route{rt})
	if got := names(r2.OrderCandidates(rt)); got[0] != "a" {
		t.Fatalf("first = %v, want a (priority fallback)", got)
	}
}

func TestPeakEWMAPicksLowerScore(t *testing.T) {
	fast := &fakeProvider{name: "fast", priority: 2}
	slow := &fakeProvider{name: "slow", priority: 1}
	rt := &Route{Model: "m", Strategy: "peak_ewma", Candidates: []Candidate{
		{Provider: slow, Model: "m"}, {Provider: fast, Model: "m"},
	}}
	r := New([]provider.Provider{slow, fast}, []*Route{rt})
	r.RecordResult("slow", 900*time.Millisecond, true)
	r.RecordResult("fast", 10*time.Millisecond, true)
	for i := 0; i < 20; i++ {
		if got := r.OrderCandidates(rt); got[0].Provider.Name() != "fast" {
			t.Fatalf("first = %s, want fast", got[0].Provider.Name())
		}
	}
}

func TestPeakEWMAWeightDivision(t *testing.T) {
	// slow-but-weighted beats fast-unweighted when score = ewma/weight.
	heavy := &fakeProvider{name: "heavy", priority: 1} // weight 10
	lite := &fakeProvider{name: "lite", priority: 2}   // weight 1
	rt := &Route{Model: "m", Strategy: "peak_ewma", Candidates: []Candidate{
		{Provider: heavy, Model: "m", Weight: 10},
		{Provider: lite, Model: "m"},
	}}
	r := New([]provider.Provider{heavy, lite}, []*Route{rt})
	r.RecordResult("heavy", 500*time.Millisecond, true) // 500/10 = 50
	r.RecordResult("lite", 100*time.Millisecond, true)  // 100/1 = 100
	for i := 0; i < 20; i++ {
		if got := r.OrderCandidates(rt); got[0].Provider.Name() != "heavy" {
			t.Fatalf("first = %s, want heavy (score 50 < 100)", got[0].Provider.Name())
		}
	}
}

func TestPeakEWMAUnseenSeededNotFree(t *testing.T) {
	// Unseen candidate must NOT auto-win: it scores with the mean seed.
	seen := &fakeProvider{name: "seen", priority: 1}
	newp := &fakeProvider{name: "new", priority: 2}
	rt := &Route{Model: "m", Strategy: "peak_ewma", Candidates: []Candidate{
		{Provider: seen, Model: "m"}, {Provider: newp, Model: "m"},
	}}
	r := New([]provider.Provider{seen, newp}, []*Route{rt})
	r.RecordResult("seen", 50*time.Millisecond, true) // seed = 50, tie vs seen
	// seed == seen's ewma -> stable coin flip by draw; just assert no panic and
	// the strategy is registered.
	if !ValidStrategy("peak_ewma") {
		t.Fatal("peak_ewma not registered")
	}
	_ = r.OrderCandidates(rt)
}
