package router

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempJSONL(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func TestLoadRoutingDataset_MetaHeaderAndRows(t *testing.T) {
	path := writeTempJSONL(t, `{"_meta": {"embedding_model": "longformer-base-4096", "embedding_dim": 768, "judge": "claude-opus", "judge_method": "pairwise_winrate"}}
{"query": "fix the bug", "best_model": "qwen-coder", "scores": {"qwen-coder": 0.92, "qwen-chat": 0.45}}
{"query": "hello there", "best_model": "qwen-chat", "scores": {"qwen-coder": 0.41, "qwen-chat": 0.83}}
`)
	ds, err := LoadRoutingDataset(path)
	if err != nil {
		t.Fatalf("LoadRoutingDataset: %v", err)
	}
	if ds.Meta.EmbeddingModel != "longformer-base-4096" {
		t.Errorf("meta.embedding_model = %q, want longformer-base-4096", ds.Meta.EmbeddingModel)
	}
	if ds.Meta.JudgeMethod != "pairwise_winrate" {
		t.Errorf("meta.judge_method = %q, want pairwise_winrate", ds.Meta.JudgeMethod)
	}
	if len(ds.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(ds.Rows))
	}
	if ds.Rows[0].BestModel != "qwen-coder" || ds.Rows[0].Scores["qwen-chat"] != 0.45 {
		t.Errorf("first row mismatch: %+v", ds.Rows[0])
	}
}

func TestLoadRoutingDataset_HeaderOptional(t *testing.T) {
	// Files without a _meta header are valid — the loader treats
	// the first line as a data row.
	path := writeTempJSONL(t, `{"query": "hello", "best_model": "qwen-chat"}
`)
	ds, err := LoadRoutingDataset(path)
	if err != nil {
		t.Fatalf("LoadRoutingDataset: %v", err)
	}
	if ds.Meta.EmbeddingModel != "" {
		t.Errorf("expected empty meta, got %+v", ds.Meta)
	}
	if len(ds.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(ds.Rows))
	}
}

func TestLoadRoutingDataset_BlankAndCommentLines(t *testing.T) {
	path := writeTempJSONL(t, `# routing dataset built 2026-05-12

{"_meta": {"embedding_model": "longformer-base-4096"}}

# generated from production traffic sample
{"query": "x", "best_model": "m1"}
`)
	ds, err := LoadRoutingDataset(path)
	if err != nil {
		t.Fatalf("LoadRoutingDataset: %v", err)
	}
	if len(ds.Rows) != 1 || ds.Rows[0].BestModel != "m1" {
		t.Errorf("rows = %+v, want 1 row of m1", ds.Rows)
	}
}

func TestLoadRoutingDataset_RejectsMissingFields(t *testing.T) {
	path := writeTempJSONL(t, `{"query": "x"}
`)
	_, err := LoadRoutingDataset(path)
	if err == nil || !strings.Contains(err.Error(), "best_model") {
		t.Errorf("expected best_model error, got %v", err)
	}
}

func TestLoadRoutingDataset_RejectsMalformedJSON(t *testing.T) {
	path := writeTempJSONL(t, `not valid json
`)
	_, err := LoadRoutingDataset(path)
	if err == nil {
		t.Error("expected parse error")
	}
}

func TestLoadRoutingDataset_MissingFile(t *testing.T) {
	_, err := LoadRoutingDataset(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRoutingDataset_FilterByCandidates(t *testing.T) {
	ds := &RoutingDataset{Rows: []RoutingRow{
		{Query: "a", BestModel: "qwen-coder"},
		{Query: "b", BestModel: "qwen-chat"},
		{Query: "c", BestModel: "gpt-5"}, // model absent from candidates
	}}
	kept, dropped := ds.FilterByCandidates([]string{"qwen-coder", "qwen-chat"})
	if len(kept) != 2 {
		t.Errorf("kept = %d, want 2", len(kept))
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

func TestRoutingDataset_EmbeddingsMatch(t *testing.T) {
	ds := &RoutingDataset{Meta: DatasetMeta{EmbeddingModel: "longformer", EmbeddingDim: 768}}
	if !ds.EmbeddingsMatch("longformer", 768) {
		t.Error("matching model+dim should be true")
	}
	if ds.EmbeddingsMatch("bge-large", 768) {
		t.Error("different model should not match")
	}
	if ds.EmbeddingsMatch("longformer", 1024) {
		t.Error("mismatched dim should not match")
	}
	// Empty router-side info → don't match (we have no claim to
	// compare against).
	if ds.EmbeddingsMatch("", 0) {
		t.Error("empty router-side embedding model should not claim a match")
	}
}

// --- KNN integration with a RoutingDataset ---

func TestKNNClassifier_SeedsFromDataset(t *testing.T) {
	// Two candidates, no hand-written Examples. Dataset rows pair
	// queries with best_model values that match each candidate's
	// Model. The classifier must seed from the dataset and route
	// nearest-neighbour correctly.
	emb := newStubEmbedder()
	cands := []KNNCandidate{
		{Label: "code", Model: "qwen-coder"},
		{Label: "weather", Model: "qwen-weather"},
	}
	ds := &RoutingDataset{Rows: []RoutingRow{
		{Query: "code question", BestModel: "qwen-coder"},
		{Query: "weather forecast", BestModel: "qwen-weather"},
	}}
	c := NewKNNClassifier(cands, emb, newStore(), 0.0, KNNOptions{Dataset: ds})
	d, err := c.Classify(context.Background(), Probe{Prompt: "show me the code"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.Label != "code" {
		t.Errorf("Label = %q, want code", d.Label)
	}
}

func TestKNNClassifier_DatasetDropsUnknownModels(t *testing.T) {
	// Row whose best_model isn't in any candidate is silently
	// dropped — admins may share one benchmark file across
	// deployments with different model lineups.
	emb := newStubEmbedder()
	cands := []KNNCandidate{
		{Label: "code", Model: "qwen-coder"},
	}
	ds := &RoutingDataset{Rows: []RoutingRow{
		{Query: "code question", BestModel: "qwen-coder"},
		{Query: "irrelevant row", BestModel: "gpt-5-not-configured"},
	}}
	c := NewKNNClassifier(cands, emb, newStore(), 0.0, KNNOptions{Dataset: ds})
	if _, err := c.Classify(context.Background(), Probe{Prompt: "code please"}); err != nil {
		t.Errorf("Classify should succeed with only known-model rows seeded: %v", err)
	}
}

func TestKNNClassifier_UsesPrecomputedEmbeddings(t *testing.T) {
	// Dataset _meta.embedding_model matches the classifier's
	// configured embedder, and rows carry an Embedding field. The
	// loader must skip Embed calls for those rows.
	emb := newStubEmbedder()
	cands := []KNNCandidate{
		{Label: "code", Model: "qwen-coder"},
	}
	ds := &RoutingDataset{
		Meta: DatasetMeta{EmbeddingModel: "test-embedder"},
		Rows: []RoutingRow{
			{Query: "row 1", BestModel: "qwen-coder", Embedding: []float32{1, 0, 0}},
			{Query: "row 2", BestModel: "qwen-coder", Embedding: []float32{0.9, 0.1, 0}},
		},
	}
	c := NewKNNClassifier(cands, emb, newStore(), 0.0, KNNOptions{
		Dataset:            ds,
		EmbeddingModelName: "test-embedder",
	})
	beforeCalls := emb.calls
	if _, err := c.Classify(context.Background(), Probe{Prompt: "code"}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	// Only the probe should have been embedded; the two rows used
	// their precomputed vectors.
	if delta := emb.calls - beforeCalls; delta != 1 {
		t.Errorf("expected 1 embed call (probe), got %d — precomputed embeddings not honoured", delta)
	}
}

func TestKNNClassifier_PrecomputedIgnoredWhenModelMismatch(t *testing.T) {
	// If the dataset was built with embedder A and the router is
	// configured with embedder B, the stored vectors live in a
	// different space — using them would yield meaningless cosine
	// scores. The loader must re-embed.
	emb := newStubEmbedder()
	cands := []KNNCandidate{
		{Label: "code", Model: "qwen-coder"},
	}
	ds := &RoutingDataset{
		Meta: DatasetMeta{EmbeddingModel: "stale-embedder"},
		Rows: []RoutingRow{
			{Query: "code question", BestModel: "qwen-coder", Embedding: []float32{99, 99, 99}},
		},
	}
	c := NewKNNClassifier(cands, emb, newStore(), 0.0, KNNOptions{
		Dataset:            ds,
		EmbeddingModelName: "current-embedder",
	})
	beforeCalls := emb.calls
	if _, err := c.Classify(context.Background(), Probe{Prompt: "code"}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if delta := emb.calls - beforeCalls; delta < 2 {
		t.Errorf("expected re-embed (>=2 calls), got %d — stale embeddings were used", delta)
	}
}

func TestKNNClassifier_HandwrittenAndDatasetCombined(t *testing.T) {
	// Both sources coexist: candidate.Examples seeds some rows,
	// dataset seeds more. The store ends up with both.
	emb := newStubEmbedder()
	cands := []KNNCandidate{
		{Label: "code", Model: "qwen-coder", Examples: []string{"code review"}},
	}
	ds := &RoutingDataset{Rows: []RoutingRow{
		{Query: "code question", BestModel: "qwen-coder"},
	}}
	store := newStore()
	c := NewKNNClassifier(cands, emb, store, 0.0, KNNOptions{Dataset: ds})
	if _, err := c.Classify(context.Background(), Probe{Prompt: "code please"}); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	// Two exemplars seeded (one from Examples, one from dataset).
	if len(store.keys) != 2 {
		t.Errorf("store seeded with %d entries, want 2", len(store.keys))
	}
}
