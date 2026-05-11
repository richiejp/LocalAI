package router

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mudler/LocalAI/pkg/store/local"
)

// KNNCandidate is the duck-typed view of config.RouterCandidate the
// KNN classifier needs. Each candidate is identified by Label and
// supplies a non-empty list of Examples — short prompts whose
// embedding clusters around what should route to this label.
//
// The router package stays core/config-free (no cycle through
// config → router → config); callers translate their RouterCandidate
// slice into KNNCandidate at construction.
type KNNCandidate struct {
	Label    string
	Examples []string
}

// Embedder produces a vector representation of a piece of text. The
// router package owns no model loader — callers wire an
// implementation that hits whatever embedding backend they're using.
// Interface keeps the package free of core/backend imports and makes
// the classifier trivially unit-testable with a stub.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// KNNClassifier picks a label by embedding the probe and finding
// the nearest exemplar's label. Exemplar embeddings are computed
// lazily on first Classify so a transient embedding-model failure
// at startup doesn't block the rest of the routing wiring.
type KNNClassifier struct {
	candidates []KNNCandidate
	embedder   Embedder

	// MinScore is the cosine-similarity floor below which the
	// classifier returns no-match (handing the middleware to the
	// fallback path). 0 disables the floor — every nearest example
	// wins regardless of similarity. Useful when the probe is wildly
	// off-topic from any exemplar set.
	minScore float32

	// store holds the loaded exemplar index. atomic.Pointer so the
	// post-load fast path is a relaxed atomic load — Classify hits
	// a hot mutex on every routed request otherwise. The first
	// (and any retry after a failed) load runs under loadMu so
	// concurrent first-Classify calls don't double-embed.
	store  atomic.Pointer[local.Store]
	loadMu sync.Mutex
}

// NewKNNClassifier panics on an empty candidate slice — same shape
// as NewFeatureClassifier; surfaces config bugs at startup rather
// than at request time. A candidate with empty Examples panics for
// the same reason: a label with no exemplars can never win and is
// almost certainly a copy-paste error.
func NewKNNClassifier(candidates []KNNCandidate, embedder Embedder, minScore float32) *KNNClassifier {
	if len(candidates) == 0 {
		panic("router/knn: at least one candidate is required")
	}
	if embedder == nil {
		panic("router/knn: embedder is required (configure router.embedding_model)")
	}
	for _, c := range candidates {
		if len(c.Examples) == 0 {
			panic(fmt.Sprintf("router/knn: candidate %q has no examples", c.Label))
		}
	}
	return &KNNClassifier{
		candidates: candidates,
		embedder:   embedder,
		minScore:   minScore,
	}
}

func (k *KNNClassifier) Name() string { return ClassifierKNN }

func (k *KNNClassifier) Classify(ctx context.Context, p Probe) (Decision, error) {
	start := time.Now()
	store, err := k.ensureLoaded(ctx)
	if err != nil {
		return errDecision(start, err)
	}

	queryVec, err := k.embedder.Embed(ctx, p.Prompt)
	if err != nil {
		return errDecision(start, fmt.Errorf("embed probe: %w", err))
	}
	if len(queryVec) == 0 {
		return errDecision(start, fmt.Errorf("embed probe: empty result"))
	}
	if store.Dim() != -1 && len(queryVec) != store.Dim() {
		return errDecision(start, fmt.Errorf("embed probe: dimension %d differs from exemplars (%d) — different embedding model?", len(queryVec), store.Dim()))
	}

	_, values, sims, err := store.Find(local.Normalize(queryVec), 1)
	if err != nil {
		return errDecision(start, fmt.Errorf("knn find: %w", err))
	}
	if len(values) == 0 {
		return errDecision(start, fmt.Errorf("knn: no exemplars in store"))
	}
	if sims[0] < k.minScore {
		return errDecision(start, fmt.Errorf("knn: best match %.3f below min_score %.3f", sims[0], k.minScore))
	}
	return Decision{
		Label:   string(values[0]),
		Score:   float64(sims[0]),
		Latency: time.Since(start),
	}, nil
}

// ensureLoaded embeds every candidate's exemplars on first call
// and returns the loaded store. Subsequent calls take a relaxed
// atomic load. After a failed attempt the next call retries;
// concurrent retries are coalesced by loadMu.
func (k *KNNClassifier) ensureLoaded(ctx context.Context) (*local.Store, error) {
	if s := k.store.Load(); s != nil {
		return s, nil
	}
	k.loadMu.Lock()
	defer k.loadMu.Unlock()
	if s := k.store.Load(); s != nil {
		return s, nil
	}
	store := local.New()
	var keys [][]float32
	var values [][]byte
	for _, c := range k.candidates {
		for _, ex := range c.Examples {
			vec, err := k.embedder.Embed(ctx, ex)
			if err != nil {
				return nil, fmt.Errorf("embed exemplar %q for label %q: %w", ex, c.Label, err)
			}
			keys = append(keys, local.Normalize(vec))
			values = append(values, []byte(c.Label))
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no exemplars to load")
	}
	if err := store.Set(keys, values); err != nil {
		return nil, fmt.Errorf("seed store: %w", err)
	}
	k.store.Store(store)
	return store, nil
}
