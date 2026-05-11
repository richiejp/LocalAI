package application

import (
	"context"

	"github.com/mudler/LocalAI/core/backend"
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/services/routing/router"
	"github.com/mudler/LocalAI/pkg/model"
	"github.com/mudler/LocalAI/pkg/store"
)

// adapterConfig resolves a model name to its runtime ModelConfig
// for the router-side adapters, or nil when the name is unknown.
// Centralised so EmbedderFactory and LLMCallerFactory share one
// config-resolution path — adding a new adapter is a 3-line factory.
func (a *Application) adapterConfig(modelName string) *config.ModelConfig {
	cfg, err := a.backendLoader.LoadModelConfigFileByNameDefaultOptions(modelName, a.applicationConfig)
	if err != nil || cfg == nil {
		return nil
	}
	return cfg
}

// EmbedderFactory returns a router.Embedder bound to the named
// embedding model, or nil when the model is not loadable. Used by
// the RouteModel middleware to wire the KNN classifier without
// importing core/backend (which would create a cycle through
// config → router → config).
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

// modelEmbedder is a thin adapter from router.Embedder to
// core/backend.ModelEmbedding.
type modelEmbedder struct {
	modelLoader *model.ModelLoader
	modelConfig *config.ModelConfig
	appConfig   *config.ApplicationConfig
}

// Embed blocks on the embedding-backend round-trip; the router
// counts this as part of its classification latency.
func (m *modelEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	fn, err := backend.ModelEmbedding(text, nil, m.modelLoader, *m.modelConfig, m.appConfig)
	if err != nil {
		return nil, err
	}
	return fn()
}

// LLMCallerFactory returns a router.LLMCaller bound to the named
// instruct model, or nil when the model is not loadable.
func (a *Application) LLMCallerFactory() func(modelName string) router.LLMCaller {
	return func(modelName string) router.LLMCaller {
		cfg := a.adapterConfig(modelName)
		if cfg == nil {
			return nil
		}
		return &modelLLMCaller{
			modelLoader:  a.modelLoader,
			configLoader: a.backendLoader,
			modelConfig:  cfg,
			appConfig:    a.applicationConfig,
		}
	}
}

type modelLLMCaller struct {
	modelLoader  *model.ModelLoader
	configLoader *config.ModelConfigLoader
	modelConfig  *config.ModelConfig
	appConfig    *config.ApplicationConfig
}

// VectorStoreFactory returns a router.VectorStore bound to a named
// store-backend model + namespace pair, or nil when the backend is
// not loadable. The router package consumes this through the
// VectorStoreFactory middleware-level alias so it stays free of
// core/backend imports. Empty backendName defaults to the in-process
// local-store gRPC backend (matches what /v1/stores uses).
func (a *Application) VectorStoreFactory() func(backendName, namespace string) router.VectorStore {
	return func(backendName, namespace string) router.VectorStore {
		return &routerVectorStore{
			ml:        a.modelLoader,
			appConfig: a.applicationConfig,
			storeName: namespace,
			backend:   backendName,
		}
	}
}

// routerVectorStore is the adapter from router.VectorStore to
// core/backend.StoreBackend + pkg/store helpers. The backend is
// resolved lazily on every call rather than cached so a transient
// load failure during the first Set retries on the next attempt,
// matching the lazy-seed pattern in KNNClassifier.ensureSeeded.
type routerVectorStore struct {
	ml        *model.ModelLoader
	appConfig *config.ApplicationConfig
	storeName string
	backend   string
}

func (r *routerVectorStore) Set(ctx context.Context, keys [][]float32, values [][]byte) error {
	b, err := backend.StoreBackend(r.ml, r.appConfig, r.storeName, r.backend)
	if err != nil {
		return err
	}
	return store.SetCols(ctx, b, keys, values)
}

func (r *routerVectorStore) Find(ctx context.Context, query []float32, topK int) ([][]byte, []float32, error) {
	b, err := backend.StoreBackend(r.ml, r.appConfig, r.storeName, r.backend)
	if err != nil {
		return nil, nil, err
	}
	_, values, sims, err := store.Find(ctx, b, query, topK)
	if err != nil {
		return nil, nil, err
	}
	return values, sims, nil
}

func (m *modelLLMCaller) Complete(ctx context.Context, system, user string) (string, error) {
	prompt := system + "\n\nUser: " + user + "\n\nLabel:"
	fn, err := backend.ModelInferenceFunc(
		ctx,
		prompt,
		nil,
		nil, nil, nil,
		m.modelLoader,
		m.modelConfig,
		m.configLoader,
		m.appConfig,
		nil,
		"", "",
		nil, nil,
		nil,
		nil,
	)
	if err != nil {
		return "", err
	}
	resp, err := fn()
	if err != nil {
		return "", err
	}
	return resp.Response, nil
}
