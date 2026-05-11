package router

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// stubEmbedder maps text to a deterministic vector by stuffing the
// caller-supplied vectors keyed on substring match. Real cosine
// similarity is preserved (vectors are user-supplied 3-D unit
// vectors), so the KNN ranking behaviour is exercised end-to-end
// without spawning an embedding backend.
type stubEmbedder struct {
	mappings map[string][]float32
	calls    int
	failNext bool
}

func (s *stubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	s.calls++
	if s.failNext {
		s.failNext = false
		return nil, errors.New("embed: backend unavailable")
	}
	for needle, vec := range s.mappings {
		if strings.Contains(text, needle) {
			out := make([]float32, len(vec))
			copy(out, vec)
			return out, nil
		}
	}
	// No match → return a vector orthogonal to every mapping (the
	// last unit basis), letting tests verify min_score gating.
	return []float32{0, 0, 1}, nil
}

func newStubEmbedder() *stubEmbedder {
	return &stubEmbedder{
		mappings: map[string][]float32{
			"code":     {1, 0, 0},
			"weather":  {0, 1, 0},
			"forecast": {0, 1, 0}, // weather-adjacent
		},
	}
}

// stubVectorStore is an in-test cosine-KNN over float32 vectors —
// independent of the gRPC local-store backend so the router tests
// stay hermetic. Keeps the same Set/Find shape the production
// adapter wraps over pkg/store.{SetCols,Find}.
type stubVectorStore struct {
	keys   [][]float32
	values [][]byte
}

func (s *stubVectorStore) Set(_ context.Context, keys [][]float32, values [][]byte) error {
	s.keys = append(s.keys, keys...)
	s.values = append(s.values, values...)
	return nil
}

func (s *stubVectorStore) Find(_ context.Context, query []float32, topK int) ([][]byte, []float32, error) {
	type item struct {
		sim float32
		val []byte
	}
	items := make([]item, 0, len(s.keys))
	for i, k := range s.keys {
		if len(k) != len(query) {
			return nil, nil, errors.New("stubVectorStore: dim mismatch")
		}
		var dot float64
		for j := range k {
			dot += float64(k[j]) * float64(query[j])
		}
		items = append(items, item{sim: float32(dot), val: s.values[i]})
	}
	slices.SortFunc(items, func(a, b item) int {
		if a.sim > b.sim {
			return -1
		}
		if a.sim < b.sim {
			return 1
		}
		return 0
	})
	if topK > len(items) {
		topK = len(items)
	}
	values := make([][]byte, topK)
	sims := make([]float32, topK)
	for i := 0; i < topK; i++ {
		values[i] = items[i].val
		sims[i] = items[i].sim
	}
	return values, sims, nil
}

func newStore() *stubVectorStore { return &stubVectorStore{} }

func TestKNNClassifier_PicksNearestExemplarLabel(t *testing.T) {
	emb := newStubEmbedder()
	cands := []KNNCandidate{
		{Label: "code", Examples: []string{"code question"}},
		{Label: "weather", Examples: []string{"weather forecast"}},
	}
	c := NewKNNClassifier(cands, emb, newStore(), 0.0)

	d, err := c.Classify(context.Background(), Probe{Prompt: "show me the code"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.Label != "code" {
		t.Errorf("Label = %q, want code", d.Label)
	}
	if d.Score < 0.99 {
		t.Errorf("Score = %v, want ~1.0", d.Score)
	}
}

func TestKNNClassifier_EmbedsExemplarsOnce(t *testing.T) {
	emb := newStubEmbedder()
	cands := []KNNCandidate{
		{Label: "code", Examples: []string{"code question", "code review"}},
		{Label: "weather", Examples: []string{"weather forecast"}},
	}
	c := NewKNNClassifier(cands, emb, newStore(), 0.0)

	if _, err := c.Classify(context.Background(), Probe{Prompt: "code please"}); err != nil {
		t.Fatal(err)
	}
	firstCalls := emb.calls // 3 exemplars + 1 probe = 4
	if firstCalls != 4 {
		t.Fatalf("first Classify: expected 4 embed calls (3 exemplars + 1 probe), got %d", firstCalls)
	}

	if _, err := c.Classify(context.Background(), Probe{Prompt: "weather please"}); err != nil {
		t.Fatal(err)
	}
	if delta := emb.calls - firstCalls; delta != 1 {
		t.Errorf("second Classify: expected 1 additional embed call (probe only), got %d", delta)
	}
}

func TestKNNClassifier_EmbedderFailureAtLoadIsRetryable(t *testing.T) {
	// A transient embedding-backend failure during exemplar load
	// must NOT permanently brick the classifier. Subsequent calls
	// retry the load, recovering once the backend comes back.
	emb := newStubEmbedder()
	emb.failNext = true
	cands := []KNNCandidate{
		{Label: "code", Examples: []string{"code question"}},
	}
	c := NewKNNClassifier(cands, emb, newStore(), 0.0)

	if _, err := c.Classify(context.Background(), Probe{Prompt: "code"}); err == nil {
		t.Fatal("first Classify should surface load error")
	}
	if _, err := c.Classify(context.Background(), Probe{Prompt: "code"}); err != nil {
		t.Errorf("second Classify should succeed after backend recovery: %v", err)
	}
}

func TestKNNClassifier_MinScoreFiltersWeakMatches(t *testing.T) {
	// Probe has no semantic match (stubEmbedder returns the
	// orthogonal basis); with min_score above 0 the classifier
	// must error out so the middleware falls back.
	emb := newStubEmbedder()
	cands := []KNNCandidate{
		{Label: "code", Examples: []string{"code question"}},
	}
	c := NewKNNClassifier(cands, emb, newStore(), 0.5)

	_, err := c.Classify(context.Background(), Probe{Prompt: "totally unrelated"})
	if err == nil {
		t.Error("expected min_score gate to reject; got match")
	}
}

func TestKNNClassifier_DimensionMismatchIsErrored(t *testing.T) {
	// Different embedders produce different dimensions; if the
	// probe-side dimension diverges from exemplars (typically a
	// config bug pointing the router at the wrong embedding model
	// after a restart), surface a clear error rather than a
	// confusing length mismatch from the underlying store.
	emb := &stubEmbedder{
		mappings: map[string][]float32{
			"code":  {1, 0, 0},
			"probe": {1, 0}, // 2-D — mismatch with the 3-D exemplar
		},
	}
	cands := []KNNCandidate{
		{Label: "code", Examples: []string{"code question"}},
	}
	c := NewKNNClassifier(cands, emb, newStore(), 0.0)

	_, err := c.Classify(context.Background(), Probe{Prompt: "probe text"})
	if err == nil || !strings.Contains(err.Error(), "dimension") {
		t.Errorf("expected dimension error, got %v", err)
	}
}

func TestKNNClassifier_PanicsOnEmptyExamples(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for empty examples")
		}
	}()
	NewKNNClassifier(
		[]KNNCandidate{{Label: "code", Examples: nil}},
		newStubEmbedder(),
		newStore(),
		0.0,
	)
}

func TestKNNClassifier_PanicsOnNilEmbedder(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil embedder")
		}
	}()
	NewKNNClassifier(
		[]KNNCandidate{{Label: "code", Examples: []string{"x"}}},
		nil,
		newStore(),
		0.0,
	)
}

func TestKNNClassifier_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil store")
		}
	}()
	NewKNNClassifier(
		[]KNNCandidate{{Label: "code", Examples: []string{"x"}}},
		newStubEmbedder(),
		nil,
		0.0,
	)
}
