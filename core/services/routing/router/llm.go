package router

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// LLMCandidate is the duck-typed view of config.RouterCandidate the
// LLM classifier needs. Description is the natural-language hint
// fed to the small classifier model alongside the probe — it should
// be one or two short sentences describing what kind of prompt
// routes here.
type LLMCandidate struct {
	Label       string
	Description string
}

// LLMCaller runs a one-shot text completion. The router package
// owns no model loader — callers wire an implementation that hits
// whatever LLM backend they're using (typically a small instruct
// model). Same shape as Embedder: keeps the package free of
// core/backend imports and stub-friendly for unit tests.
type LLMCaller interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// LLMClassifier asks a small LLM "which of these labels best fits
// this prompt" and parses the response. A per-prompt cache amortises
// the LLM round-trip across repeat probes — useful for agent loops
// that re-classify near-identical prompts each step.
type LLMClassifier struct {
	candidates []LLMCandidate
	caller     LLMCaller

	// systemPrompt is built once at construction. The prompt lists
	// every label + description and pins the response shape so a
	// chatty model still produces a parseable answer.
	systemPrompt string

	// labels maps lower-case → canonical for O(1) direct-hit lookup.
	// orderedLabels is the same set sorted longest-first so the
	// substring fallback picks "code-review" before "code" when
	// both are valid candidates — map-iteration order is randomised
	// and would otherwise be a non-deterministic ambiguity.
	labels        map[string]string
	orderedLabels []string

	// cache stores label-by-(normalised prompt) to skip the LLM
	// round-trip on repeat queries. RWMutex so cache hits don't
	// contend on a write lock under concurrent classification.
	mu       sync.RWMutex
	cache    map[string]string
	cacheCap int
}

// NewLLMClassifier panics on caller errors at construction (empty
// candidate list, missing description, nil caller) — same rationale
// as the other classifiers. cacheCap=0 disables the cache.
func NewLLMClassifier(candidates []LLMCandidate, caller LLMCaller, cacheCap int) *LLMClassifier {
	if len(candidates) == 0 {
		panic("router/llm: at least one candidate is required")
	}
	if caller == nil {
		panic("router/llm: caller is required (configure router.classifier_model)")
	}
	for _, c := range candidates {
		if c.Description == "" {
			panic(fmt.Sprintf("router/llm: candidate %q has no description", c.Label))
		}
	}
	labels := make(map[string]string, len(candidates))
	ordered := make([]string, 0, len(candidates))
	for _, c := range candidates {
		lower := strings.ToLower(c.Label)
		labels[lower] = c.Label
		ordered = append(ordered, lower)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	if cacheCap < 0 {
		cacheCap = 0
	}
	return &LLMClassifier{
		candidates:    candidates,
		caller:        caller,
		systemPrompt:  buildLLMSystemPrompt(candidates),
		labels:        labels,
		orderedLabels: ordered,
		cache:         make(map[string]string, cacheCap),
		cacheCap:      cacheCap,
	}
}

func (c *LLMClassifier) Name() string { return ClassifierLLM }

func (c *LLMClassifier) Classify(ctx context.Context, p Probe) (Decision, error) {
	start := time.Now()
	if hit, ok := c.lookupCache(p.Prompt); ok {
		return Decision{Label: hit, Score: 1.0, Latency: time.Since(start)}, nil
	}
	raw, err := c.caller.Complete(ctx, c.systemPrompt, p.Prompt)
	if err != nil {
		return errDecision(start, fmt.Errorf("llm classify: %w", err))
	}
	label, ok := c.parseLabel(raw)
	if !ok {
		return errDecision(start, fmt.Errorf("llm classify: response %q does not match any candidate label", strings.TrimSpace(raw)))
	}
	c.storeCache(p.Prompt, label)
	return Decision{
		Label:   label,
		Score:   1.0,
		Latency: time.Since(start),
	}, nil
}

// parseLabel scans the LLM response for a candidate label
// (case-insensitive). Tolerates leading/trailing whitespace, code
// fences, and chatty preamble like "I'd pick: code".
//
// Substring fallback walks labels longest-first so "code-review"
// is matched before "code" when both are configured.
func (c *LLMClassifier) parseLabel(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if canon, ok := c.labels[s]; ok {
		return canon, true
	}
	for _, lower := range c.orderedLabels {
		if strings.Contains(s, lower) {
			return c.labels[lower], true
		}
	}
	return "", false
}

// cacheKey collapses incidental whitespace and casing so prompts
// like "hello", " hello ", and "Hello" share an entry — agent loops
// frequently produce minor variations that would otherwise miss.
func cacheKey(prompt string) string {
	return strings.ToLower(strings.TrimSpace(prompt))
}

func (c *LLMClassifier) lookupCache(prompt string) (string, bool) {
	if c.cacheCap == 0 {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.cache[cacheKey(prompt)]
	return v, ok
}

func (c *LLMClassifier) storeCache(prompt, label string) {
	if c.cacheCap == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= c.cacheCap {
		// Random eviction (map iteration order). Cheaper than LRU;
		// fine while typical prompt sets stay smaller than cap.
		for k := range c.cache {
			delete(c.cache, k)
			break
		}
	}
	c.cache[cacheKey(prompt)] = label
}

// CacheLen returns the number of cached prompts. Test-only API —
// avoids leaking the internal map shape.
func (c *LLMClassifier) CacheLen() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

func buildLLMSystemPrompt(candidates []LLMCandidate) string {
	var b strings.Builder
	b.WriteString("You are a routing classifier. Given a user prompt, pick the SINGLE label whose description best fits.\n\n")
	b.WriteString("Available labels:\n")
	for _, c := range candidates {
		b.WriteString("- ")
		b.WriteString(c.Label)
		b.WriteString(": ")
		b.WriteString(c.Description)
		b.WriteString("\n")
	}
	b.WriteString("\nRespond with ONLY the label, no explanation, no quotes, no formatting.")
	return b.String()
}
