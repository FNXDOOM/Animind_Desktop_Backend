package torrent

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrendingCounter(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tr := NewTrending(func() time.Time { return now })
	tr.Record("  Sousou   no FRIEREN!! ")
	tr.Record("sousou no frieren")
	tr.Record("x")   // too short, ignored
	tr.Record("   ") // ignored
	top := tr.Top(10)
	if len(top) != 1 || top[0] != "sousou no frieren" {
		t.Fatalf("top: %v", top)
	}
	now = now.Add(8 * 24 * time.Hour) // outside the 7-day window
	if got := tr.Top(10); len(got) != 0 {
		t.Fatalf("window: %v", got)
	}
}

func TestTrendingCountsAndTieOrder(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tr := NewTrending(func() time.Time { return now })
	tr.Record("bb")
	tr.Record("bb")
	tr.Record("bb")
	tr.Record("aa")
	tr.Record("cc")
	// Descending by count; equal counts settle alphabetically because the buckets are a
	// map and Top must stay deterministic.
	if got := tr.Top(10); !reflect.DeepEqual(got, []string{"bb", "aa", "cc"}) {
		t.Fatalf("top: %v, want [bb aa cc]", got)
	}
	if got := tr.Top(2); !reflect.DeepEqual(got, []string{"bb", "aa"}) {
		t.Fatalf("top(2): %v", got)
	}
	if got := tr.Top(0); got != nil {
		t.Fatalf("top(0): %v", got)
	}
}

func TestTrendingNormalizesAndBounds(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tr := NewTrending(func() time.Time { return now })
	tr.Record("  Hello,   WORLD!!! ")
	tr.Record(strings.Repeat("あ", 80)) // the upper bound is accepted
	if got := tr.Top(10); len(got) != 2 || got[0] != "hello world" {
		t.Fatalf("normalization: %v", got)
	}
	tr.Record(strings.Repeat("あ", 81)) // one rune over is ignored
	tr.Record("a")                     // one rune under is ignored
	if got := tr.Top(10); len(got) != 2 {
		t.Fatalf("bounds: %v", got)
	}
}

func TestTrendingWindowBoundary(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tr := NewTrending(func() time.Time { return now })
	tr.Record("frieren")
	now = now.Add(6 * 24 * time.Hour) // the record's day is the oldest day of the window
	if got := tr.Top(10); len(got) != 1 {
		t.Fatalf("six days on must still count: %v", got)
	}
	now = now.Add(1 * 24 * time.Hour) // day seven falls out of the 7-day window
	if got := tr.Top(10); len(got) != 0 {
		t.Fatalf("seven days on must be empty: %v", got)
	}
}

func TestTrendingConcurrentRecords(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tr := NewTrending(func() time.Time { return now })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Record("frieren")
			tr.Top(10)
		}()
	}
	wg.Wait()
	if got := tr.Top(10); len(got) != 1 || got[0] != "frieren" {
		t.Fatalf("concurrent records: %v", got)
	}
}
