package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// RoutingDataset is the artefact a router-benchmarking pipeline emits:
// one row per query, with the best-performing model + optional
// per-model scores + optional pre-computed embedding. The KNN
// classifier consumes it as exemplars whose label is the candidate
// matching best_model.
//
// The shape mirrors what a pairwise-LLM-judge or task-specific
// scorer benchmark would produce (see the LLMRouter project for one
// such pipeline). LocalAI doesn't ship the benchmarker yet — this
// type defines the file format so a benchmarking CLI can land later
// without changing the consumer.
type RoutingDataset struct {
	Meta DatasetMeta
	Rows []RoutingRow
}

// DatasetMeta is the optional header row. Lets the loader detect
// embedding-model mismatch and lets admins see at a glance what
// produced the file. All fields are optional.
type DatasetMeta struct {
	// EmbeddingModel names the model whose embeddings are stored
	// in the rows' Embedding field. If unset (or doesn't match the
	// router's configured embedder), the KNN classifier re-embeds
	// queries at load time.
	EmbeddingModel string `json:"embedding_model,omitempty"`

	// EmbeddingDim is the dimension of stored embeddings — used
	// for a sanity check before seeding the store.
	EmbeddingDim int `json:"embedding_dim,omitempty"`

	// Judge identifies the scoring method ("claude-opus pairwise",
	// "gsm8k exact-match", etc.). Free-form audit trail.
	Judge string `json:"judge,omitempty"`

	// JudgeMethod is the scorer category — "pairwise_winrate",
	// "exact_match", "code_eval", etc. Lets a downstream consumer
	// pick the right confidence interpretation.
	JudgeMethod string `json:"judge_method,omitempty"`

	// CreatedAt is when the benchmarking pipeline produced the
	// file. Helps admins spot stale routing data.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// RoutingRow is one query × scoring outcome. BestModel is the
// candidate model name that scored highest; the KNN classifier
// translates this into one of the configured RouterCandidate
// labels by matching candidate.Model == BestModel.
type RoutingRow struct {
	// Query is the prompt this row represents.
	Query string `json:"query"`

	// BestModel is the candidate model name that scored highest
	// for this query under the benchmark. Required.
	BestModel string `json:"best_model"`

	// Scores is the optional per-model score (0..1 range, exact
	// semantics depend on the benchmark — win-rate, accuracy,
	// pass@k, etc.). Used by score-weighted KNN variants; the
	// current 1-NN consumer reads only BestModel.
	Scores map[string]float64 `json:"scores,omitempty"`

	// Embedding is the optional pre-computed query embedding.
	// When the dataset's _meta.embedding_model matches the
	// router's configured embedder, the loader uses this directly
	// and skips per-query embed calls — a 10x-100x cold-start
	// speedup for large datasets.
	Embedding []float32 `json:"embedding,omitempty"`

	// TaskHint is an optional taxonomy label ("code", "math",
	// "chat") the benchmarking pipeline may attach. Surfaced in
	// the decision log so admins can see what category of query
	// activated a route.
	TaskHint string `json:"task_hint,omitempty"`
}

// LoadRoutingDataset parses a JSONL file into a RoutingDataset.
// File format: one JSON object per line, optional `{"_meta": {...}}`
// header on the first line. Blank lines and comment lines starting
// with '#' are skipped. Returns an empty dataset (no error) for an
// empty file — lets callers ship a placeholder while the benchmarker
// is being run.
func LoadRoutingDataset(path string) (*RoutingDataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open exemplars file %q: %w", path, err)
	}
	defer f.Close()

	ds := &RoutingDataset{}
	scanner := bufio.NewScanner(f)
	// Default scanner buffer maxes at 64KB. A row with an inline
	// pre-computed embedding can blow past that — a 1024-D
	// jina-v3 vector serialised as JSON is already ~10KB; some
	// models output 4096-D. Bump to 8MB per line with the same
	// 64KB initial allocation, so small rows don't pay the
	// memory cost up front.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	lineNum := 0
	seenFirstData := false
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// First non-blank line: probe for a _meta header. Anything
		// else is a data row.
		if !seenFirstData {
			var headerProbe struct {
				Meta *DatasetMeta `json:"_meta,omitempty"`
			}
			if err := json.Unmarshal([]byte(line), &headerProbe); err == nil && headerProbe.Meta != nil {
				ds.Meta = *headerProbe.Meta
				seenFirstData = true
				continue
			}
			seenFirstData = true
		}
		var row RoutingRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse line %d: %w", lineNum, err)
		}
		if row.Query == "" {
			return nil, fmt.Errorf("line %d: empty query", lineNum)
		}
		if row.BestModel == "" {
			return nil, fmt.Errorf("line %d: missing best_model", lineNum)
		}
		ds.Rows = append(ds.Rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read exemplars file %q: %w", path, err)
	}
	return ds, nil
}

// FilterByCandidates drops rows whose BestModel doesn't match any
// configured candidate. Returns the kept-rows slice and the count
// of dropped rows. Lets the router benchmark against a broader
// model lineup than the deployment uses without rebuilding the
// dataset — entries for absent models are silently ignored.
func (d *RoutingDataset) FilterByCandidates(candidateModels []string) (kept []RoutingRow, dropped int) {
	if d == nil {
		return nil, 0
	}
	want := make(map[string]struct{}, len(candidateModels))
	for _, m := range candidateModels {
		want[m] = struct{}{}
	}
	for _, r := range d.Rows {
		if _, ok := want[r.BestModel]; !ok {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	return kept, dropped
}

// EmbeddingsMatch reports whether the dataset's stored embeddings
// align with the router's configured embedder. Used by the KNN
// loader to decide whether to skip per-query Embed calls.
func (d *RoutingDataset) EmbeddingsMatch(embeddingModel string, expectedDim int) bool {
	if d == nil || d.Meta.EmbeddingModel == "" || embeddingModel == "" {
		return false
	}
	if d.Meta.EmbeddingModel != embeddingModel {
		return false
	}
	if expectedDim != 0 && d.Meta.EmbeddingDim != 0 && expectedDim != d.Meta.EmbeddingDim {
		return false
	}
	return true
}

