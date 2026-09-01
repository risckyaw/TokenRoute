package server

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestRateLimitReset(t *testing.T) {
	future := time.Now().Add(90 * time.Second)
	cases := []struct {
		name string
		h    http.Header
		want time.Duration
	}{
		{"absent", http.Header{}, 0},
		{"seconds duration", http.Header{"X-Ratelimit-Reset-Requests": []string{"120"}}, 120 * time.Second},
		{"unix timestamp", http.Header{"X-Ratelimit-Reset": []string{strconv.FormatInt(future.Unix(), 10)}}, 90 * time.Second},
		{"http date", http.Header{"X-Ratelimit-Reset": []string{future.UTC().Format(http.TimeFormat)}}, 90 * time.Second},
		{"garbage", http.Header{"X-Ratelimit-Reset": []string{"soon"}}, 0},
		{"requests wins", http.Header{
			"X-Ratelimit-Reset-Requests": []string{"45"},
			"X-Ratelimit-Reset":          []string{"999"},
		}, 45 * time.Second},
		{"capped at 24h", http.Header{"X-Ratelimit-Reset": []string{"999999"}}, 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rateLimitReset(tc.h)
			if tc.want == 0 {
				if got != 0 {
					t.Fatalf("want 0, got %v", got)
				}
				return
			}
			// Time-derived values drift by execution time; allow 5s slack.
			diff := got - tc.want
			if diff < 0 {
				diff = -diff
			}
			if diff > 5*time.Second {
				t.Fatalf("want ~%v, got %v", tc.want, got)
			}
		})
	}
}
