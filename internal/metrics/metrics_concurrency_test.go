package metrics

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestBindGaugesConcurrentWithWrite(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			n := i
			r.BindGauges(
				func() []string { return []string{fmt.Sprintf("p%d", n)} },
				func(string) bool { return n%2 == 0 },
				func(string) int { return n },
			)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			var b bytes.Buffer
			r.Write(&b)
		}
	}()
	wg.Wait()
}
