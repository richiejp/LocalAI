package router

import (
	"context"
	"fmt"
	"time"
)

// RerankResult mirrors the rerankers backend's per-document score at
// the router boundary. Lives here so the router can depend on this
// abstraction without importing core/backend. Document text is NOT
// echoed back — the classifier only needs (Index, RelevanceScore) to
// align scores with the input order.
type RerankResult struct {
	Index          int
	RelevanceScore float32
}

// Reranker scores a list of candidate documents against a query and
// returns each document's relevance. The router treats every policy
// label's description as a candidate document and reads off the
// per-label relevance to decide the active label set.
//
// LocalAI's rerankers backend already supports cross-encoder,
// late-interaction (ColBERT), and bge-m3 multi-vector heads natively
// via the Python `rerankers` library — selection between them is a
// model choice (config.ModelType passed through as model_type), so
// this interface is uniform across all three.
type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string) ([]RerankResult, error)
}

// RerankClassifier asks a reranker model to score each policy
// description against the prompt and activates the labels whose
// relevance scores are top-K and clear an absolute floor. Same shape
// as ScoreClassifier but using cross-attention / late-interaction
// scoring rather than next-token log-probabilities.
//
// Use case: small classifier-tuned LMs (Arch-Router-style) struggle
// with abstract policy labels because the labels aren't typical next
// tokens of the user prompt. Reranker-style scoring scores the
// *description* directly, which is more robust when descriptions are
// the natural English the reranker was trained on.
type RerankClassifier struct {
	policies            []ScorePolicy
	reranker            Reranker
	activationThreshold float64

	// labelOrder + descriptionOrder are aligned: descriptionOrder[i]
	// is the document the reranker scores; labelOrder[i] is the
	// label associated with that document.
	labelOrder       []string
	descriptionOrder []string

	cache *labelSetCache
}

// defaultRerankActivationThreshold is the relevance-score floor a
// label must clear to be considered active. Reranker scores live
// in [0, 1] for cross-encoders / ColBERT; 0.5 picks "the model is
// more positive than not on this label." Lower for noisier
// rerankers, higher for cleaner ones.
const defaultRerankActivationThreshold = 0.5

// NewRerankClassifier panics on caller errors at construction
// (empty policies, missing description, nil reranker) — same
// rationale as ScoreClassifier. cacheCap=0 disables the cache.
// activationThreshold=0 picks the package default.
func NewRerankClassifier(policies []ScorePolicy, reranker Reranker, cacheCap int, activationThreshold float64) *RerankClassifier {
	if len(policies) == 0 {
		panic("router/rerank: at least one policy is required")
	}
	if reranker == nil {
		panic("router/rerank: reranker is required (configure router.classifier_model)")
	}
	for _, p := range policies {
		if p.Label == "" {
			panic("router/rerank: policy has empty label")
		}
		if p.Description == "" {
			panic(fmt.Sprintf("router/rerank: policy %q has no description", p.Label))
		}
	}
	labels := make([]string, 0, len(policies))
	descriptions := make([]string, 0, len(policies))
	for _, p := range policies {
		labels = append(labels, p.Label)
		descriptions = append(descriptions, p.Description)
	}
	if activationThreshold <= 0 {
		activationThreshold = defaultRerankActivationThreshold
	}
	return &RerankClassifier{
		policies:            policies,
		reranker:            reranker,
		activationThreshold: activationThreshold,
		labelOrder:          labels,
		descriptionOrder:    descriptions,
		cache:               newLabelSetCache(cacheCap),
	}
}

func (c *RerankClassifier) Name() string { return ClassifierColbert }

func (c *RerankClassifier) Classify(ctx context.Context, p Probe) (Decision, error) {
	start := time.Now()
	if hit, ok := c.cache.lookup(p.Prompt); ok {
		return Decision{Labels: hit, Score: 1.0, Latency: time.Since(start)}, nil
	}

	results, err := c.reranker.Rerank(ctx, p.Prompt, c.descriptionOrder)
	if err != nil {
		return errDecision(start, fmt.Errorf("rerank classify: %w", err))
	}

	// The reranker may return fewer-than-N entries (top_n filtering)
	// or reorder them by score. Materialise into a label→score map
	// so threshold + argmax don't depend on result ordering.
	scoreByLabel := make(map[string]float64, len(c.labelOrder))
	for _, r := range results {
		if r.Index < 0 || r.Index >= len(c.labelOrder) {
			continue
		}
		scoreByLabel[c.labelOrder[r.Index]] = float64(r.RelevanceScore)
	}

	active := make([]string, 0, len(c.labelOrder))
	bestIdx := 0
	for i, label := range c.labelOrder {
		score := scoreByLabel[label]
		if score > scoreByLabel[c.labelOrder[bestIdx]] {
			bestIdx = i
		}
		if score >= c.activationThreshold {
			active = append(active, label)
		}
	}
	// Defensive: if no label crossed threshold, fall back to argmax
	// so the caller always has something to route on. Mirrors
	// ScoreClassifier on flat distributions. Argmax is well-defined
	// even when every score is 0 — the first label wins.
	if len(active) == 0 {
		active = []string{c.labelOrder[bestIdx]}
	}
	c.cache.put(p.Prompt, active)
	return Decision{
		Labels:  active,
		Score:   scoreByLabel[c.labelOrder[bestIdx]],
		Latency: time.Since(start),
	}, nil
}

// CacheLen returns the number of cached prompts. Test-only API.
func (c *RerankClassifier) CacheLen() int { return c.cache.count() }
