package torrent

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Trending counts normalized search queries over a rolling 7-day window. It is in-process
// by design (spec §7.3): with several replicas each reports its own counts, and no user
// identifier is ever recorded — only the query text itself.
type Trending struct {
	mu   sync.Mutex
	days map[string]map[string]int // UTC date -> normalized query -> hits
	now  func() time.Time
}

// NewTrending builds the counter; now defaults to time.Now and is injectable for tests.
func NewTrending(now func() time.Time) *Trending {
	if now == nil {
		now = time.Now
	}
	return &Trending{days: make(map[string]map[string]int), now: now}
}

const trendingDayFormat = "2006-01-02"

// Record counts one search query. The query is normalized (trim, lowercase, strip
// punctuation, collapse whitespace) and ignored outside the 2–80 rune range, so control
// characters and one-letter floods never reach the endpoint.
func (t *Trending) Record(query string) {
	q := normalizeTrendingQuery(query)
	if n := utf8.RuneCountInString(q); n < 2 || n > 80 {
		return
	}
	day := t.now().UTC().Format(trendingDayFormat)
	t.mu.Lock()
	defer t.mu.Unlock()
	bucket, ok := t.days[day]
	if !ok {
		bucket = make(map[string]int)
		t.days[day] = bucket
	}
	bucket[q]++
}

// Top returns up to n queries hit inside the last seven days (today inclusive), descending
// by count. Equal counts settle alphabetically: the buckets are a map, so Top must break
// ties itself to stay deterministic.
func (t *Trending) Top(n int) []string {
	if n <= 0 {
		return nil
	}
	cutoff := t.now().UTC().AddDate(0, 0, -6).Format(trendingDayFormat) // seven days incl. today
	t.mu.Lock()
	counts := make(map[string]int)
	for day, bucket := range t.days {
		if day < cutoff {
			delete(t.days, day) // prune while holding the lock; expired days are unreachable
			continue
		}
		for q, hits := range bucket {
			counts[q] += hits
		}
	}
	t.mu.Unlock()

	queries := make([]string, 0, len(counts))
	for q := range counts {
		queries = append(queries, q)
	}
	sort.Slice(queries, func(i, j int) bool {
		if counts[queries[i]] != counts[queries[j]] {
			return counts[queries[i]] > counts[queries[j]]
		}
		return queries[i] < queries[j]
	})
	if len(queries) > n {
		queries = queries[:n]
	}
	return queries
}

// normalizeTrendingQuery strips punctuation and symbols, then collapses whitespace; the
// removal can leave double spaces behind ("a - b"), so the collapse runs last.
func normalizeTrendingQuery(q string) string {
	stripped := strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return r
	}, strings.ToLower(q))
	return strings.Join(strings.Fields(stripped), " ")
}
