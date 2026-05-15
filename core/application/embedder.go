package application

import (
	"context"
	"fmt"
	"strings"

	"github.com/mudler/LocalAI/core/backend"
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/services/routing/router"
	"github.com/mudler/LocalAI/pkg/grpc"
	"github.com/mudler/LocalAI/pkg/grpc/proto"
	"github.com/mudler/LocalAI/pkg/model"
	"github.com/mudler/LocalAI/pkg/store"
)

// adapterConfig resolves a model name to its runtime ModelConfig
// for the router-side adapter, or nil when the name is unknown.
func (a *Application) adapterConfig(modelName string) *config.ModelConfig {
	cfg, err := a.backendLoader.LoadModelConfigFileByNameDefaultOptions(modelName, a.applicationConfig)
	if err != nil || cfg == nil {
		return nil
	}
	return cfg
}

// ScorerFactory returns a router.Scorer bound to the named model, or
// nil when the model is not loadable. The router uses this to obtain
// joint log-probabilities of policy labels under the configured
// classifier model — multi-label routing without asking the model to
// emit a single argmax label (which off-the-shelf classifier-tuned
// models like Arch-Router struggle with via grammar constraint).
func (a *Application) ScorerFactory() func(modelName string) router.Scorer {
	return func(modelName string) router.Scorer {
		cfg := a.adapterConfig(modelName)
		if cfg == nil {
			return nil
		}
		return &modelScorer{
			modelLoader: a.modelLoader,
			modelConfig: cfg,
			appConfig:   a.applicationConfig,
		}
	}
}

type modelScorer struct {
	modelLoader *model.ModelLoader
	modelConfig *config.ModelConfig
	appConfig   *config.ApplicationConfig
}

func (m *modelScorer) Score(ctx context.Context, prompt string, candidates []string) ([]router.CandidateScore, error) {
	fn, err := backend.ModelScore(prompt, candidates, backend.ScoreOptions{LengthNormalize: true}, m.modelLoader, *m.modelConfig, m.appConfig)
	if err != nil {
		return nil, err
	}
	raw, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]router.CandidateScore, len(raw))
	for i, c := range raw {
		out[i] = router.CandidateScore{
			LogProb:                 c.LogProb,
			LengthNormalizedLogProb: c.LengthNormalizedLogProb,
			NumTokens:               c.NumTokens,
		}
	}
	return out, nil
}

// RerankerFactory returns a router.Reranker bound to the named model,
// or nil when the model is not loadable. The colbert classifier uses
// this to rerank policy descriptions against the prompt — the
// reranker model's `type:` (e.g. "colbert") selects the underlying
// scoring algorithm in the rerankers backend.
func (a *Application) RerankerFactory() func(modelName string) router.Reranker {
	return func(modelName string) router.Reranker {
		cfg := a.adapterConfig(modelName)
		if cfg == nil {
			return nil
		}
		return &modelReranker{
			modelLoader: a.modelLoader,
			modelConfig: cfg,
			appConfig:   a.applicationConfig,
		}
	}
}

type modelReranker struct {
	modelLoader *model.ModelLoader
	modelConfig *config.ModelConfig
	appConfig   *config.ApplicationConfig
}

func (r *modelReranker) Rerank(ctx context.Context, query string, documents []string) ([]router.RerankResult, error) {
	req := &proto.RerankRequest{
		Query:     query,
		Documents: documents,
		// TopN=0 → return scores for every document. The classifier
		// needs every label scored; truncating top-N would silently
		// zero out labels the reranker considered unlikely.
	}
	res, err := backend.Rerank(ctx, req, r.modelLoader, r.appConfig, *r.modelConfig)
	if err != nil {
		return nil, err
	}
	out := make([]router.RerankResult, 0, len(res.GetResults()))
	for _, dr := range res.GetResults() {
		out = append(out, router.RerankResult{
			Index:          int(dr.GetIndex()),
			RelevanceScore: dr.GetRelevanceScore(),
		})
	}
	return out, nil
}

// EmbedderFactory returns a router.Embedder bound to the named model,
// or nil when the model is not loadable. The L2 embedding cache uses
// this to embed router probes before searching the vector store.
func (a *Application) EmbedderFactory() func(modelName string) router.Embedder {
	return func(modelName string) router.Embedder {
		cfg := a.adapterConfig(modelName)
		if cfg == nil {
			return nil
		}
		return &modelEmbedder{
			modelLoader: a.modelLoader,
			modelConfig: cfg,
			appConfig:   a.applicationConfig,
		}
	}
}

type modelEmbedder struct {
	modelLoader *model.ModelLoader
	modelConfig *config.ModelConfig
	appConfig   *config.ApplicationConfig
}

func (e *modelEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	fn, err := backend.ModelEmbedding(text, nil, e.modelLoader, *e.modelConfig, e.appConfig)
	if err != nil {
		return nil, err
	}
	return fn()
}

// VectorStoreFactory returns a router.VectorStore bound to the named
// collection. The local-store backend is loaded once per collection
// name; each router model gets its own backend process via the
// model.ModelLoader cache keyed by storeName.
func (a *Application) VectorStoreFactory() func(storeName string) router.VectorStore {
	return func(storeName string) router.VectorStore {
		if storeName == "" {
			return nil
		}
		return &localVectorStore{
			appConfig:   a.applicationConfig,
			modelLoader: a.modelLoader,
			storeName:   storeName,
		}
	}
}

type localVectorStore struct {
	appConfig   *config.ApplicationConfig
	modelLoader *model.ModelLoader
	storeName   string
}

func (s *localVectorStore) backend(ctx context.Context) (grpc.Backend, error) {
	_ = ctx // local-store load is synchronous; ctx unused here for symmetry with the interface.
	return backend.StoreBackend(s.modelLoader, s.appConfig, s.storeName, "")
}

func (s *localVectorStore) Search(ctx context.Context, vec []float32) (float64, []byte, bool, error) {
	be, err := s.backend(ctx)
	if err != nil {
		return 0, nil, false, fmt.Errorf("vector store load: %w", err)
	}
	_, values, similarities, err := store.Find(ctx, be, vec, 1)
	if err != nil {
		// local-store's Find returns "existing length is -1" when no
		// keys have been inserted yet. Surface that as a clean miss so
		// the cache layer doesn't treat it as a failure and skip the
		// follow-up Insert.
		if strings.Contains(err.Error(), "existing length is -1") {
			return 0, nil, false, nil
		}
		return 0, nil, false, fmt.Errorf("vector store find: %w", err)
	}
	if len(values) == 0 || len(similarities) == 0 {
		return 0, nil, false, nil
	}
	return float64(similarities[0]), values[0], true, nil
}

func (s *localVectorStore) Insert(ctx context.Context, vec []float32, payload []byte) error {
	be, err := s.backend(ctx)
	if err != nil {
		return fmt.Errorf("vector store load: %w", err)
	}
	return store.SetSingle(ctx, be, vec, payload)
}
