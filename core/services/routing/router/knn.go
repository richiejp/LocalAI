package router

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
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

// VectorStore is the surface KNN needs from a pluggable vector store
// backend (local-store, qdrant, pinecone, ...). The concrete
// implementation lives outside this package — the application wires
// a thin adapter over pkg/store's gRPC client. Returning to the
// router package as an interface keeps the classifier free of
// core/backend imports and matches the indirection used for
// face/voice recognition.
//
// Find returns the topK nearest entries ordered most-similar first,
// with the byte values being whatever the caller stored (KNN stores
// candidate label strings). Sims[i] aligns with Values[i].
type VectorStore interface {
	Set(ctx context.Context, keys [][]float32, values [][]byte) error
	Find(ctx context.Context, query []float32, topK int) (values [][]byte, sims []float32, err error)
}

// KNNClassifier picks a label by embedding the probe and finding
// the nearest exemplar's label. Exemplar embeddings are computed
// lazily on first Classify so a transient embedding-model failure
// at startup doesn't block the rest of the routing wiring.
type KNNClassifier struct {
	candidates []KNNCandidate
	embedder   Embedder
	store      VectorStore

	// MinScore is the cosine-similarity floor below which the
	// classifier returns no-match (handing the middleware to the
	// fallback path). 0 disables the floor — every nearest example
	// wins regardless of similarity. Useful when the probe is wildly
	// off-topic from any exemplar set.
	minScore float32

	// seeded flips to non-nil once the exemplar set has been embedded
	// and pushed to the backing store. Lazy because the embedding
	// model and vector store backends may not be reachable at
	// startup. atomic.Bool so the fast path is a relaxed load; the
	// first (and any retry after a failed) seed runs under seedMu
	// to coalesce concurrent first-Classify calls.
	seeded atomic.Bool
	seedMu sync.Mutex
	// exemplarDim memoises the dimension of the seeded exemplars so
	// Classify can fail fast on a dimension mismatch — would otherwise
	// surface as a confusing low-similarity result.
	exemplarDim int
}

// NewKNNClassifier panics on an empty candidate slice — same shape
// as NewFeatureClassifier; surfaces config bugs at startup rather
// than at request time. A candidate with empty Examples panics for
// the same reason: a label with no exemplars can never win and is
// almost certainly a copy-paste error.
func NewKNNClassifier(candidates []KNNCandidate, embedder Embedder, store VectorStore, minScore float32) *KNNClassifier {
	if len(candidates) == 0 {
		panic("router/knn: at least one candidate is required")
	}
	if embedder == nil {
		panic("router/knn: embedder is required (configure router.embedding_model)")
	}
	if store == nil {
		panic("router/knn: vector store is required (configure router.store_model)")
	}
	for _, c := range candidates {
		if len(c.Examples) == 0 {
			panic(fmt.Sprintf("router/knn: candidate %q has no examples", c.Label))
		}
	}
	return &KNNClassifier{
		candidates: candidates,
		embedder:   embedder,
		store:      store,
		minScore:   minScore,
	}
}

func (k *KNNClassifier) Name() string { return ClassifierKNN }

func (k *KNNClassifier) Classify(ctx context.Context, p Probe) (Decision, error) {
	start := time.Now()
	if err := k.ensureSeeded(ctx); err != nil {
		return errDecision(start, err)
	}

	queryVec, err := k.embedder.Embed(ctx, p.Prompt)
	if err != nil {
		return errDecision(start, fmt.Errorf("embed probe: %w", err))
	}
	if len(queryVec) == 0 {
		return errDecision(start, fmt.Errorf("embed probe: empty result"))
	}
	if k.exemplarDim != 0 && len(queryVec) != k.exemplarDim {
		return errDecision(start, fmt.Errorf("embed probe: dimension %d differs from exemplars (%d) — different embedding model?", len(queryVec), k.exemplarDim))
	}

	values, sims, err := k.store.Find(ctx, normalize(queryVec), 1)
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

// ensureSeeded embeds every candidate's exemplars on first call and
// pushes them to the backing vector store. Subsequent calls take a
// relaxed atomic load. After a failed attempt the next call retries;
// concurrent retries are coalesced by seedMu.
func (k *KNNClassifier) ensureSeeded(ctx context.Context) error {
	if k.seeded.Load() {
		return nil
	}
	k.seedMu.Lock()
	defer k.seedMu.Unlock()
	if k.seeded.Load() {
		return nil
	}
	var keys [][]float32
	var values [][]byte
	for _, c := range k.candidates {
		for _, ex := range c.Examples {
			vec, err := k.embedder.Embed(ctx, ex)
			if err != nil {
				return fmt.Errorf("embed exemplar %q for label %q: %w", ex, c.Label, err)
			}
			if len(vec) == 0 {
				return fmt.Errorf("embed exemplar %q for label %q: empty result", ex, c.Label)
			}
			if k.exemplarDim == 0 {
				k.exemplarDim = len(vec)
			} else if len(vec) != k.exemplarDim {
				return fmt.Errorf("embed exemplar %q for label %q: dimension %d differs from prior exemplars (%d)", ex, c.Label, len(vec), k.exemplarDim)
			}
			keys = append(keys, normalize(vec))
			values = append(values, []byte(c.Label))
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("no exemplars to load")
	}
	if err := k.store.Set(ctx, keys, values); err != nil {
		return fmt.Errorf("seed store: %w", err)
	}
	k.seeded.Store(true)
	return nil
}

// normalize returns a copy of v with unit magnitude, or v unchanged
// when its magnitude is already zero or close-to-one. Normalising
// before insert and query keeps the local-store fast path engaged
// and lets the cosine similarity score returned by the store stand
// in as a plain dot product.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	mag := math.Sqrt(sum)
	if mag == 0 || (mag >= 0.99 && mag <= 1.01) {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / mag)
	}
	return out
}
