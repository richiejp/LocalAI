package backend

import (
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/schema"
	"github.com/mudler/LocalAI/pkg/grpc"
	"github.com/mudler/LocalAI/pkg/model"
)

func ModelTokenize(s string, loader *model.ModelLoader, modelConfig config.ModelConfig) (schema.TokenizeResponse, error) {

	var inferenceModel grpc.Backend
	var err error

	appConfig := loader.ApplicationConfig()
	opts := ModelOptions(modelConfig, appConfig)
	inferenceModel, err = loader.Load(opts...)
	if err != nil {
		return schema.TokenizeResponse{}, err
	}

	predictOptions := model.GrpcPredictOpts(modelConfig, loader.ModelPath)
	predictOptions.Prompt = s

	// tokenize the string
	resp, err := inferenceModel.TokenizeString(appConfig.Context, predictOptions)
	if err != nil {
		return schema.TokenizeResponse{}, err
	}

	if resp.Tokens == nil {
		resp.Tokens = make([]int32, 0)
	}

	return schema.TokenizeResponse{
		Tokens: resp.Tokens,
	}, nil

}
