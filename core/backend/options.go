package backend

import (
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/pkg/model"
)

func ModelOptions(c config.ModelConfig, so *config.ApplicationConfig, opts ...model.Option) []model.Option {
	name := c.Name
	if name == "" {
		name = c.Model
	}

	defOpts := []model.Option{
		model.WithBackendString(c.Backend),
		model.WithModel(c.Model),
		model.WithContext(so.Context),
		model.WithModelID(name),
		model.WithModelConfig(c),
		model.WithApplicationConfig(so),
	}

	threads := 1

	if c.Threads != nil {
		threads = *c.Threads
	}

	if so.Threads != 0 {
		threads = so.Threads
	}

	c.Threads = &threads

	grpcOpts := model.GrpcModelOpts(c, so.SystemState.Model.ModelsPath)
	defOpts = append(defOpts, model.WithLoadGRPCLoadModelOpts(grpcOpts))

	if so.ParallelBackendRequests {
		defOpts = append(defOpts, model.EnableParallelRequests)
	}

	if c.GRPC.Attempts != 0 {
		defOpts = append(defOpts, model.WithGRPCAttempts(c.GRPC.Attempts))
	}

	if c.GRPC.AttemptsSleepTime != 0 {
		defOpts = append(defOpts, model.WithGRPCAttemptsDelay(c.GRPC.AttemptsSleepTime))
	}

	for k, v := range so.ExternalGRPCBackends {
		defOpts = append(defOpts, model.WithExternalBackend(k, v))
	}

	return append(defOpts, opts...)
}
