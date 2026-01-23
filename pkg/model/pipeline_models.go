package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mudler/LocalAI/core/config"
	grpcClient "github.com/mudler/LocalAI/pkg/grpc"
	pb "github.com/mudler/LocalAI/pkg/grpc/proto"
	"github.com/mudler/LocalAI/pkg/utils"
	"github.com/mudler/xlog"
	"google.golang.org/grpc"
)

// PipelineModel is a wrapper that implements grpc.Backend but uses multiple models for different operations
// It corresponds to the "wrappedModel" from the realtime implementation
type PipelineModel struct {
	TTSConfig           *config.ModelConfig
	TranscriptionConfig *config.ModelConfig
	LLMConfig           *config.ModelConfig
	TTSClient           grpcClient.Backend
	TranscriptionClient grpcClient.Backend
	LLMClient           grpcClient.Backend

	VADConfig *config.ModelConfig
	VADClient grpcClient.Backend
	appConfig *config.ApplicationConfig
}

func (m *PipelineModel) IsBusy() bool {
	return aggregateIsBusy(m.LLMClient, m.TTSClient, m.TranscriptionClient, m.VADClient)
}

func (m *PipelineModel) HealthCheck(ctx context.Context) (bool, error) {
	if m.LLMClient != nil {
		if ok, err := m.LLMClient.HealthCheck(ctx); !ok || err != nil {
			return ok, err
		}
	}
	if m.TTSClient != nil {
		if ok, err := m.TTSClient.HealthCheck(ctx); !ok || err != nil {
			return ok, err
		}
	}
	if m.TranscriptionClient != nil {
		if ok, err := m.TranscriptionClient.HealthCheck(ctx); !ok || err != nil {
			return ok, err
		}
	}
	if m.VADClient != nil {
		if ok, err := m.VADClient.HealthCheck(ctx); !ok || err != nil {
			return ok, err
		}
	}
	return true, nil
}

func (m *PipelineModel) LoadModel(ctx context.Context, in *pb.ModelOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	// Models are loaded during initialization of PipelineModel
	return &pb.Result{Success: true}, nil
}

func (m *PipelineModel) Predict(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.Reply, error) {
	if m.LLMClient == nil {
		return nil, fmt.Errorf("LLM component not available in pipeline")
	}
	return m.LLMClient.Predict(ctx, in, opts...)
}

func (m *PipelineModel) PredictStream(ctx context.Context, in *pb.PredictOptions, f func(reply *pb.Reply), opts ...grpc.CallOption) error {
	if m.LLMClient == nil {
		return fmt.Errorf("LLM component not available in pipeline")
	}
	return m.LLMClient.PredictStream(ctx, in, f, opts...)
}

func (m *PipelineModel) AudioTranscription(ctx context.Context, in *pb.TranscriptRequest, opts ...grpc.CallOption) (*pb.TranscriptResult, error) {
	if m.TranscriptionClient == nil {
		return nil, fmt.Errorf("transcription component not available in pipeline")
	}
	return m.TranscriptionClient.AudioTranscription(ctx, in, opts...)
}

func (m *PipelineModel) TTS(ctx context.Context, in *pb.TTSRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	if m.TTSClient == nil {
		return nil, fmt.Errorf("TTS component not available in pipeline")
	}

	if m.appConfig != nil && m.appConfig.SystemState != nil && m.TTSConfig != nil {
		mp := filepath.Join(m.appConfig.SystemState.Model.ModelsPath, m.TTSConfig.Model)
		if _, err := os.Stat(mp); err == nil {
			if err := utils.VerifyPath(mp, m.appConfig.SystemState.Model.ModelsPath); err == nil {
				in.Model = mp
			}
		}
	}

	if in.Dst == "" && m.appConfig != nil {
		audioDir := filepath.Join(m.appConfig.GeneratedContentDir, "audio")
		if err := os.MkdirAll(audioDir, 0750); err != nil {
			return nil, fmt.Errorf("failed creating audio directory: %s", err)
		}

		fileName := utils.GenerateUniqueFileName(audioDir, "tts", ".wav")
		in.Dst = filepath.Join(audioDir, fileName)
	}

	return m.TTSClient.TTS(ctx, in, opts...)
}

func (m *PipelineModel) VAD(ctx context.Context, in *pb.VADRequest, opts ...grpc.CallOption) (*pb.VADResponse, error) {
	if m.VADClient == nil {
		return nil, fmt.Errorf("VAD component not available in pipeline")
	}
	return m.VADClient.VAD(ctx, in, opts...)
}

// Unsupported methods return errors
func (m *PipelineModel) Embeddings(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.EmbeddingResult, error) {
	if m.LLMClient != nil {
		return m.LLMClient.Embeddings(ctx, in, opts...)
	}
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) GenerateImage(ctx context.Context, in *pb.GenerateImageRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) GenerateVideo(ctx context.Context, in *pb.GenerateVideoRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) SoundGeneration(ctx context.Context, in *pb.SoundGenerationRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) Detect(ctx context.Context, in *pb.DetectOptions, opts ...grpc.CallOption) (*pb.DetectResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) TokenizeString(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.TokenizationResponse, error) {
	if m.LLMClient != nil {
		return m.LLMClient.TokenizeString(ctx, in, opts...)
	}
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) Status(ctx context.Context) (*pb.StatusResponse, error) {
	return aggregateStatus(ctx, m.LLMClient, m.TTSClient, m.TranscriptionClient, m.VADClient)
}
func (m *PipelineModel) StoresSet(ctx context.Context, in *pb.StoresSetOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) StoresDelete(ctx context.Context, in *pb.StoresDeleteOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) StoresGet(ctx context.Context, in *pb.StoresGetOptions, opts ...grpc.CallOption) (*pb.StoresGetResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) StoresFind(ctx context.Context, in *pb.StoresFindOptions, opts ...grpc.CallOption) (*pb.StoresFindResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) Rerank(ctx context.Context, in *pb.RerankRequest, opts ...grpc.CallOption) (*pb.RerankResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *PipelineModel) GetTokenMetrics(ctx context.Context, in *pb.MetricsRequest, opts ...grpc.CallOption) (*pb.MetricsResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

// AnyToAnyModel implementation
type AnyToAnyModel struct {
	LLMConfig *config.ModelConfig
	LLMClient grpcClient.Backend

	VADConfig *config.ModelConfig
	VADClient grpcClient.Backend
}

func (m *AnyToAnyModel) IsBusy() bool {
	return aggregateIsBusy(m.LLMClient, m.VADClient)
}

func (m *AnyToAnyModel) HealthCheck(ctx context.Context) (bool, error) {
	if m.LLMClient != nil {
		if ok, err := m.LLMClient.HealthCheck(ctx); !ok || err != nil {
			return ok, err
		}
	}
	if m.VADClient != nil {
		if ok, err := m.VADClient.HealthCheck(ctx); !ok || err != nil {
			return ok, err
		}
	}
	return true, nil
}

func (m *AnyToAnyModel) LoadModel(ctx context.Context, in *pb.ModelOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	return &pb.Result{Success: true}, nil
}

func (m *AnyToAnyModel) Predict(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.Reply, error) {
	return m.LLMClient.Predict(ctx, in, opts...)
}

func (m *AnyToAnyModel) PredictStream(ctx context.Context, in *pb.PredictOptions, f func(reply *pb.Reply), opts ...grpc.CallOption) error {
	return m.LLMClient.PredictStream(ctx, in, f, opts...)
}

func (m *AnyToAnyModel) TTS(ctx context.Context, in *pb.TTSRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return m.LLMClient.TTS(ctx, in, opts...)
}

func (m *AnyToAnyModel) AudioTranscription(ctx context.Context, in *pb.TranscriptRequest, opts ...grpc.CallOption) (*pb.TranscriptResult, error) {
	return m.LLMClient.AudioTranscription(ctx, in, opts...)
}

func (m *AnyToAnyModel) VAD(ctx context.Context, in *pb.VADRequest, opts ...grpc.CallOption) (*pb.VADResponse, error) {
	return m.VADClient.VAD(ctx, in, opts...)
}

// Delegate everything else to LLMClient
func (m *AnyToAnyModel) Embeddings(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.EmbeddingResult, error) {
	return m.LLMClient.Embeddings(ctx, in, opts...)
}
func (m *AnyToAnyModel) GenerateImage(ctx context.Context, in *pb.GenerateImageRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return m.LLMClient.GenerateImage(ctx, in, opts...)
}
func (m *AnyToAnyModel) GenerateVideo(ctx context.Context, in *pb.GenerateVideoRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return m.LLMClient.GenerateVideo(ctx, in, opts...)
}
func (m *AnyToAnyModel) SoundGeneration(ctx context.Context, in *pb.SoundGenerationRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return m.LLMClient.SoundGeneration(ctx, in, opts...)
}
func (m *AnyToAnyModel) Detect(ctx context.Context, in *pb.DetectOptions, opts ...grpc.CallOption) (*pb.DetectResponse, error) {
	return m.LLMClient.Detect(ctx, in, opts...)
}
func (m *AnyToAnyModel) TokenizeString(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.TokenizationResponse, error) {
	return m.LLMClient.TokenizeString(ctx, in, opts...)
}
func (m *AnyToAnyModel) Status(ctx context.Context) (*pb.StatusResponse, error) {
	return aggregateStatus(ctx, m.LLMClient, m.VADClient)
}
func (m *AnyToAnyModel) StoresSet(ctx context.Context, in *pb.StoresSetOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	return m.LLMClient.StoresSet(ctx, in, opts...)
}
func (m *AnyToAnyModel) StoresDelete(ctx context.Context, in *pb.StoresDeleteOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	return m.LLMClient.StoresDelete(ctx, in, opts...)
}
func (m *AnyToAnyModel) StoresGet(ctx context.Context, in *pb.StoresGetOptions, opts ...grpc.CallOption) (*pb.StoresGetResult, error) {
	return m.LLMClient.StoresGet(ctx, in, opts...)
}
func (m *AnyToAnyModel) StoresFind(ctx context.Context, in *pb.StoresFindOptions, opts ...grpc.CallOption) (*pb.StoresFindResult, error) {
	return m.LLMClient.StoresFind(ctx, in, opts...)
}
func (m *AnyToAnyModel) Rerank(ctx context.Context, in *pb.RerankRequest, opts ...grpc.CallOption) (*pb.RerankResult, error) {
	return m.LLMClient.Rerank(ctx, in, opts...)
}
func (m *AnyToAnyModel) GetTokenMetrics(ctx context.Context, in *pb.MetricsRequest, opts ...grpc.CallOption) (*pb.MetricsResponse, error) {
	return m.LLMClient.GetTokenMetrics(ctx, in, opts...)
}

// TranscriptionOnlyModel implementation
type TranscriptionOnlyModel struct {
	TranscriptionConfig *config.ModelConfig
	TranscriptionClient grpcClient.Backend
	VADConfig           *config.ModelConfig
	VADClient           grpcClient.Backend
	appConfig           *config.ApplicationConfig
}

func (m *TranscriptionOnlyModel) IsBusy() bool {
	return aggregateIsBusy(m.TranscriptionClient, m.VADClient)
}

func (m *TranscriptionOnlyModel) HealthCheck(ctx context.Context) (bool, error) {
	if m.TranscriptionClient != nil {
		if ok, err := m.TranscriptionClient.HealthCheck(ctx); !ok || err != nil {
			return ok, err
		}
	}
	if m.VADClient != nil {
		if ok, err := m.VADClient.HealthCheck(ctx); !ok || err != nil {
			return ok, err
		}
	}
	return true, nil
}

func (m *TranscriptionOnlyModel) LoadModel(ctx context.Context, in *pb.ModelOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	return &pb.Result{Success: true}, nil
}

func (m *TranscriptionOnlyModel) AudioTranscription(ctx context.Context, in *pb.TranscriptRequest, opts ...grpc.CallOption) (*pb.TranscriptResult, error) {
	return m.TranscriptionClient.AudioTranscription(ctx, in, opts...)
}

func (m *TranscriptionOnlyModel) VAD(ctx context.Context, in *pb.VADRequest, opts ...grpc.CallOption) (*pb.VADResponse, error) {
	return m.VADClient.VAD(ctx, in, opts...)
}

// Unsupported methods
func (m *TranscriptionOnlyModel) Predict(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.Reply, error) {
	return nil, fmt.Errorf("predict not supported in transcription-only mode")
}
func (m *TranscriptionOnlyModel) PredictStream(ctx context.Context, in *pb.PredictOptions, f func(reply *pb.Reply), opts ...grpc.CallOption) error {
	return fmt.Errorf("predict stream not supported in transcription-only mode")
}
func (m *TranscriptionOnlyModel) TTS(ctx context.Context, in *pb.TTSRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("TTS not supported in transcription-only mode")
}
func (m *TranscriptionOnlyModel) Embeddings(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.EmbeddingResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) GenerateImage(ctx context.Context, in *pb.GenerateImageRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) GenerateVideo(ctx context.Context, in *pb.GenerateVideoRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) SoundGeneration(ctx context.Context, in *pb.SoundGenerationRequest, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) Detect(ctx context.Context, in *pb.DetectOptions, opts ...grpc.CallOption) (*pb.DetectResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) TokenizeString(ctx context.Context, in *pb.PredictOptions, opts ...grpc.CallOption) (*pb.TokenizationResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) Status(ctx context.Context) (*pb.StatusResponse, error) {
	return aggregateStatus(ctx, m.TranscriptionClient, m.VADClient)
}
func (m *TranscriptionOnlyModel) StoresSet(ctx context.Context, in *pb.StoresSetOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) StoresDelete(ctx context.Context, in *pb.StoresDeleteOptions, opts ...grpc.CallOption) (*pb.Result, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) StoresGet(ctx context.Context, in *pb.StoresGetOptions, opts ...grpc.CallOption) (*pb.StoresGetResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) StoresFind(ctx context.Context, in *pb.StoresFindOptions, opts ...grpc.CallOption) (*pb.StoresFindResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) Rerank(ctx context.Context, in *pb.RerankRequest, opts ...grpc.CallOption) (*pb.RerankResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *TranscriptionOnlyModel) GetTokenMetrics(ctx context.Context, in *pb.MetricsRequest, opts ...grpc.CallOption) (*pb.MetricsResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

// Factory function to create a pipeline model
func NewPipelineModel(pipeline *config.Pipeline, cl *config.ModelConfigLoader, ml *ModelLoader, appConfig *config.ApplicationConfig) (grpcClient.Backend, error) {
	xlog.Debug("Creating new model pipeline model", "pipeline", pipeline)

	// VAD is required for all pipeline types in realtime context
	cfgVAD, err := cl.LoadModelConfigFileByName(pipeline.VAD, ml.ModelPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load VAD config: %w", err)
	}

	opts := BuildModelOptions(*cfgVAD, appConfig)
	VADClient, err := ml.Load(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load VAD model: %w", err)
	}

	// If it's just VAD and Transcription (transcription only)
	if pipeline.LLM == "" && pipeline.TTS == "" {
		if pipeline.Transcription == "" {
			return nil, fmt.Errorf("transcription model required for transcription-only pipeline")
		}

		cfgSST, err := cl.LoadModelConfigFileByName(pipeline.Transcription, ml.ModelPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load SST config: %w", err)
		}

		opts = BuildModelOptions(*cfgSST, appConfig)
		transcriptionClient, err := ml.Load(opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to load SST model: %w", err)
		}

		return &TranscriptionOnlyModel{
			VADConfig:           cfgVAD,
			VADClient:           VADClient,
			TranscriptionConfig: cfgSST,
			TranscriptionClient: transcriptionClient,
			appConfig:           appConfig,
		}, nil
	}

	// Full pipeline
	if pipeline.Transcription != "" && pipeline.LLM != "" && pipeline.TTS != "" {
		cfgSST, err := cl.LoadModelConfigFileByName(pipeline.Transcription, ml.ModelPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load SST config: %w", err)
		}
		opts = BuildModelOptions(*cfgSST, appConfig)
		transcriptionClient, err := ml.Load(opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to load SST model: %w", err)
		}

		cfgLLM, err := cl.LoadModelConfigFileByName(pipeline.LLM, ml.ModelPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load LLM config: %w", err)
		}
		opts = BuildModelOptions(*cfgLLM, appConfig)
		llmClient, err := ml.Load(opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to load LLM model: %w", err)
		}

		cfgTTS, err := cl.LoadModelConfigFileByName(pipeline.TTS, ml.ModelPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load TTS config: %w", err)
		}
		opts = BuildModelOptions(*cfgTTS, appConfig)
		ttsClient, err := ml.Load(opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to load TTS model: %w", err)
		}

		return &PipelineModel{
			TTSConfig:           cfgTTS,
			TranscriptionConfig: cfgSST,
			LLMConfig:           cfgLLM,
			TTSClient:           ttsClient,
			TranscriptionClient: transcriptionClient,
			LLMClient:           llmClient,
			VADConfig:           cfgVAD,
			VADClient:           VADClient,
			appConfig:           appConfig,
		}, nil
	}

	// Any-to-Any
	// If only LLM is specified (and maybe VAD)
	if pipeline.LLM != "" {
		cfgAnyToAny, err := cl.LoadModelConfigFileByName(pipeline.LLM, ml.ModelPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load LLM config: %w", err)
		}
		opts = BuildModelOptions(*cfgAnyToAny, appConfig)
		anyToAnyClient, err := ml.Load(opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to load LLM model: %w", err)
		}

		return &AnyToAnyModel{
			LLMConfig: cfgAnyToAny,
			LLMClient: anyToAnyClient,
			VADConfig: cfgVAD,
			VADClient: VADClient,
		}, nil
	}

	return nil, fmt.Errorf("invalid pipeline configuration")
}

func aggregateStatus(ctx context.Context, clients ...grpcClient.Backend) (*pb.StatusResponse, error) {
	hasError := false
	hasBusy := false
	hasUninitialized := false

	for _, c := range clients {
		if c == nil {
			continue
		}
		s, err := c.Status(ctx)
		if err != nil {
			return nil, err
		}
		switch s.State {
		case pb.StatusResponse_ERROR:
			hasError = true
		case pb.StatusResponse_BUSY:
			hasBusy = true
		case pb.StatusResponse_UNINITIALIZED:
			hasUninitialized = true
		}
	}

	if hasError {
		return &pb.StatusResponse{State: pb.StatusResponse_ERROR}, nil
	}
	if hasBusy {
		return &pb.StatusResponse{State: pb.StatusResponse_BUSY}, nil
	}
	if hasUninitialized {
		return &pb.StatusResponse{State: pb.StatusResponse_UNINITIALIZED}, nil
	}

	return &pb.StatusResponse{State: pb.StatusResponse_READY}, nil
}

func aggregateIsBusy(clients ...grpcClient.Backend) bool {
	for _, c := range clients {
		if c != nil && c.IsBusy() {
			return true
		}
	}
	return false
}
