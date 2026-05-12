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
	Label string
	// Model is the candidate's downstream model name. Used to
	// resolve dataset rows: a RoutingRow with best_model == Model
	// is loaded as an exemplar carrying this Label. Optional when
	// only hand-written Examples are configured (the legacy path).
	Model    string
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

	// dataset is the optional benchmarker-produced exemplar set —
	// {query, best_model, [embedding]} rows. Loaded from a JSONL
	// file referenced by RouterConfig.ExemplarsFile. Seeding pulls
	// rows whose best_model matches a candidate and uses the
	// candidate's Label. Nil dataset means hand-written examples
	// only (the legacy path).
	dataset *RoutingDataset

	// embeddingModelName names the embedder this classifier is
	// wired to. Compared against the dataset's _meta.embedding_model
	// to decide whether pre-computed embeddings in rows can be
	// used verbatim or must be re-embedded.
	embeddingModelName string

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

// KNNOptions threads optional construction parameters through
// NewKNNClassifier without breaking the existing positional shape.
// Today there are two: a routing dataset (benchmarker output) and
// the embedding model name (for dataset alignment checks). Either
// or both may be zero-valued.
type KNNOptions struct {
	Dataset            *RoutingDataset
	EmbeddingModelName string
}

// NewKNNClassifier panics on a clearly-broken config (empty
// candidate slice, nil embedder, nil store) — same shape as
// NewFeatureClassifier; surfaces problems at startup rather than at
// request time. Per-candidate validation is relaxed: a candidate may
// have no Examples if a dataset is supplied (the loader contributes
// the exemplars instead). If both Examples and Dataset are empty,
// the seed step fails on the first Classify and the middleware falls
// back, which is the right failure mode.
func NewKNNClassifier(candidates []KNNCandidate, embedder Embedder, store VectorStore, minScore float32, opts KNNOptions) *KNNClassifier {
	if len(candidates) == 0 {
		panic("router/knn: at least one candidate is required")
	}
	if embedder == nil {
		panic("router/knn: embedder is required (configure router.embedding_model)")
	}
	if store == nil {
		panic("router/knn: vector store is required (configure router.store_model)")
	}
	return &KNNClassifier{
		candidates:         candidates,
		embedder:           embedder,
		store:              store,
		minScore:           minScore,
		dataset:            opts.Dataset,
		embeddingModelName: opts.EmbeddingModelName,
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

// ensureSeeded embeds every exemplar on first call and pushes them
// to the backing vector store. Exemplars come from two sources, both
// optional: per-candidate Examples (hand-written) and the RoutingDataset
// (benchmarker output). Subsequent calls take a relaxed atomic load.
// After a failed attempt the next call retries; concurrent retries
// are coalesced by seedMu.
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

	// Pass 1: hand-written examples.
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

	// Pass 2: dataset rows. Each row's best_model maps to a
	// candidate's label via Model. Rows referencing models the
	// router doesn't know about are silently dropped — admins may
	// share one benchmark file across deployments with different
	// candidate lineups.
	if k.dataset != nil {
		modelToLabel := make(map[string]string, len(k.candidates))
		for _, c := range k.candidates {
			if c.Model != "" {
				modelToLabel[c.Model] = c.Label
			}
		}
		// Use pre-computed embeddings only when the dataset's
		// _meta.embedding_model matches what we're embedding probes
		// with — otherwise the rows' vectors and probe vectors
		// live in different spaces and cosine similarity is
		// meaningless.
		usePrecomputed := k.dataset.EmbeddingsMatch(k.embeddingModelName, 0)
		for _, row := range k.dataset.Rows {
			label, ok := modelToLabel[row.BestModel]
			if !ok {
				continue
			}
			var vec []float32
			if usePrecomputed && len(row.Embedding) > 0 {
				vec = row.Embedding
			} else {
				v, err := k.embedder.Embed(ctx, row.Query)
				if err != nil {
					return fmt.Errorf("embed dataset row %q: %w", row.Query, err)
				}
				vec = v
			}
			if len(vec) == 0 {
				return fmt.Errorf("dataset row %q: empty embedding", row.Query)
			}
			if k.exemplarDim == 0 {
				k.exemplarDim = len(vec)
			} else if len(vec) != k.exemplarDim {
				return fmt.Errorf("dataset row %q: dimension %d differs from prior exemplars (%d)", row.Query, len(vec), k.exemplarDim)
			}
			keys = append(keys, normalize(vec))
			values = append(values, []byte(label))
		}
	}

	if len(keys) == 0 {
		return fmt.Errorf("no exemplars to load (no Examples in candidates and no usable dataset rows)")
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
