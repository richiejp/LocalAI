package router

import (
	"sort"
	"strings"
	"sync"
)

// labelSetCache memoises classifier output (a sorted active-label set)
// keyed on the case-folded, whitespace-trimmed prompt. Both
// ScoreClassifier and RerankClassifier embed one — the cache layer
// is identical for both and pulling it out keeps the two
// implementations focused on their scoring logic.
//
// cap=0 disables the cache entirely. Eviction is naive (drop one
// arbitrary entry on overflow); the cache is a hot-prompt amortiser,
// not a long-tail store, so LRU semantics aren't worth the extra
// bookkeeping.
type labelSetCache struct {
	mu    sync.RWMutex
	store map[string][]string
	cap   int
}

func newLabelSetCache(cap int) *labelSetCache {
	if cap < 0 {
		cap = 0
	}
	return &labelSetCache{store: make(map[string][]string, cap), cap: cap}
}

func (c *labelSetCache) lookup(prompt string) ([]string, bool) {
	if c.cap == 0 {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.store[cacheKey(prompt)]
	return v, ok
}

func (c *labelSetCache) put(prompt string, labels []string) {
	if c.cap == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.store) >= c.cap {
		for k := range c.store {
			delete(c.store, k)
			break
		}
	}
	// Defensive copy + sort: cached label sets must be stable so
	// callers can't mutate via aliasing, and equality comparisons
	// in tests don't depend on insertion order.
	cp := make([]string, len(labels))
	copy(cp, labels)
	sort.Strings(cp)
	c.store[cacheKey(prompt)] = cp
}

func (c *labelSetCache) count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.store)
}

// cacheKey collapses incidental whitespace and casing so prompts like
// "hello", " hello ", and "Hello" share an entry — agent loops often
// produce minor variations that would otherwise miss.
func cacheKey(prompt string) string {
	return strings.ToLower(strings.TrimSpace(prompt))
}
