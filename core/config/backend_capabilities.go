package config

import (
	"slices"
	"strings"
)

// GRPCMethod identifies a Backend service RPC from backend.proto.
type GRPCMethod string

const (
	MethodPredict            GRPCMethod = "Predict"
	MethodPredictStream      GRPCMethod = "PredictStream"
	MethodEmbedding          GRPCMethod = "Embedding"
	MethodGenerateImage      GRPCMethod = "GenerateImage"
	MethodGenerateVideo      GRPCMethod = "GenerateVideo"
	MethodAudioTranscription GRPCMethod = "AudioTranscription"
	MethodTTS                GRPCMethod = "TTS"
	MethodTTSStream          GRPCMethod = "TTSStream"
	MethodSoundGeneration    GRPCMethod = "SoundGeneration"
	MethodTokenizeString     GRPCMethod = "TokenizeString"
	MethodDetect             GRPCMethod = "Detect"
	MethodRerank             GRPCMethod = "Rerank"
	MethodVAD                GRPCMethod = "VAD"
)

// UsecaseInfo describes a single known_usecase value and how it maps
// to the gRPC backend API.
type UsecaseInfo struct {
	// Flag is the ModelConfigUsecase bitmask value.
	Flag ModelConfigUsecase
	// GRPCMethod is the primary Backend service RPC this usecase maps to.
	GRPCMethod GRPCMethod
	// IsModifier is true when this usecase doesn't map to its own gRPC RPC
	// but modifies how another RPC behaves (e.g., vision uses Predict with images).
	IsModifier bool
	// DependsOn names the usecase(s) this modifier requires (e.g., "chat").
	DependsOn string
	// Description is a human/LLM-readable explanation of what this usecase means.
	Description string
}

// UsecaseInfoMap maps each known_usecase string to its gRPC and semantic info.
var UsecaseInfoMap = map[string]UsecaseInfo{
	"chat": {
		Flag:        FLAG_CHAT,
		GRPCMethod:  MethodPredict,
		Description: "Conversational/instruction-following via the Predict RPC with chat templates.",
	},
	"completion": {
		Flag:        FLAG_COMPLETION,
		GRPCMethod:  MethodPredict,
		Description: "Text completion via the Predict RPC with a completion template.",
	},
	"edit": {
		Flag:        FLAG_EDIT,
		GRPCMethod:  MethodPredict,
		Description: "Text editing via the Predict RPC with an edit template.",
	},
	"vision": {
		Flag:        FLAG_VISION,
		GRPCMethod:  MethodPredict,
		IsModifier:  true,
		DependsOn:   "chat",
		Description: "The model accepts images alongside text in the Predict RPC. For llama-cpp this requires an mmproj file.",
	},
	"embeddings": {
		Flag:        FLAG_EMBEDDINGS,
		GRPCMethod:  MethodEmbedding,
		Description: "Vector embedding generation via the Embedding RPC.",
	},
	"tokenize": {
		Flag:        FLAG_TOKENIZE,
		GRPCMethod:  MethodTokenizeString,
		Description: "Tokenization via the TokenizeString RPC without running inference.",
	},
	"image": {
		Flag:        FLAG_IMAGE,
		GRPCMethod:  MethodGenerateImage,
		Description: "Image generation via the GenerateImage RPC (Stable Diffusion, Flux, etc.).",
	},
	"video": {
		Flag:        FLAG_VIDEO,
		GRPCMethod:  MethodGenerateVideo,
		Description: "Video generation via the GenerateVideo RPC.",
	},
	"transcript": {
		Flag:        FLAG_TRANSCRIPT,
		GRPCMethod:  MethodAudioTranscription,
		Description: "Speech-to-text via the AudioTranscription RPC.",
	},
	"tts": {
		Flag:        FLAG_TTS,
		GRPCMethod:  MethodTTS,
		Description: "Text-to-speech via the TTS RPC.",
	},
	"sound_generation": {
		Flag:        FLAG_SOUND_GENERATION,
		GRPCMethod:  MethodSoundGeneration,
		Description: "Music/sound generation via the SoundGeneration RPC (not speech).",
	},
	"rerank": {
		Flag:        FLAG_RERANK,
		GRPCMethod:  MethodRerank,
		Description: "Document reranking via the Rerank RPC.",
	},
	"detection": {
		Flag:        FLAG_DETECTION,
		GRPCMethod:  MethodDetect,
		Description: "Object detection via the Detect RPC with bounding boxes.",
	},
	"vad": {
		Flag:        FLAG_VAD,
		GRPCMethod:  MethodVAD,
		Description: "Voice activity detection via the VAD RPC.",
	},
}

// BackendCapability describes which gRPC methods and usecases a backend supports.
// Derived from reviewing actual implementations in backend/go/ and backend/python/.
type BackendCapability struct {
	// GRPCMethods lists the Backend service RPCs this backend implements.
	GRPCMethods []GRPCMethod
	// PossibleUsecases lists all usecase strings this backend can support.
	PossibleUsecases []string
	// DefaultUsecases lists the conservative safe defaults.
	DefaultUsecases []string
	// AcceptsImages indicates multimodal image input in Predict.
	AcceptsImages bool
	// AcceptsVideos indicates multimodal video input in Predict.
	AcceptsVideos bool
	// AcceptsAudios indicates multimodal audio input in Predict.
	AcceptsAudios bool
	// Description is a human-readable summary of the backend.
	Description string
}

// BackendCapabilities maps each backend name (as used in model configs and gallery
// entries) to its verified capabilities. This is the single source of truth for
// what each backend supports.
//
// Backend names use hyphens (e.g., "llama-cpp") matching the gallery convention.
// Use NormalizeBackendName() for names with dots (e.g., "llama.cpp").
var BackendCapabilities = map[string]BackendCapability{
	// --- LLM / text generation backends ---
	"llama-cpp": {
		GRPCMethods:      []GRPCMethod{MethodPredict, MethodPredictStream, MethodEmbedding, MethodTokenizeString},
		PossibleUsecases: []string{"chat", "completion", "edit", "embeddings", "tokenize", "vision"},
		DefaultUsecases:  []string{"chat"},
		AcceptsImages:    true, // requires mmproj
		Description:      "llama.cpp GGUF models — LLM inference with optional vision via mmproj",
	},
	"vllm": {
		GRPCMethods:      []GRPCMethod{MethodPredict, MethodPredictStream, MethodEmbedding},
		PossibleUsecases: []string{"chat", "completion", "embeddings", "vision"},
		DefaultUsecases:  []string{"chat"},
		AcceptsImages:    true,
		AcceptsVideos:    true,
		Description:      "vLLM engine — high-throughput LLM serving with optional multimodal",
	},
	"vllm-omni": {
		GRPCMethods:      []GRPCMethod{MethodPredict, MethodPredictStream, MethodGenerateImage, MethodGenerateVideo, MethodTTS},
		PossibleUsecases: []string{"chat", "completion", "image", "video", "tts", "vision"},
		DefaultUsecases:  []string{"chat"},
		AcceptsImages:    true,
		AcceptsVideos:    true,
		AcceptsAudios:    true,
		Description:      "vLLM omni-modal — supports text, image, video generation and TTS",
	},
	"transformers": {
		GRPCMethods:      []GRPCMethod{MethodPredict, MethodPredictStream, MethodEmbedding, MethodTTS, MethodSoundGeneration},
		PossibleUsecases: []string{"chat", "completion", "embeddings", "tts", "sound_generation"},
		DefaultUsecases:  []string{"chat"},
		Description:      "HuggingFace transformers — general-purpose Python inference",
	},
	"mlx": {
		GRPCMethods:      []GRPCMethod{MethodPredict, MethodPredictStream, MethodEmbedding},
		PossibleUsecases: []string{"chat", "completion", "embeddings"},
		DefaultUsecases:  []string{"chat"},
		Description:      "Apple MLX framework — optimized for Apple Silicon",
	},
	"mlx-distributed": {
		GRPCMethods:      []GRPCMethod{MethodPredict, MethodPredictStream, MethodEmbedding},
		PossibleUsecases: []string{"chat", "completion", "embeddings"},
		DefaultUsecases:  []string{"chat"},
		Description:      "MLX distributed inference across multiple Apple Silicon devices",
	},
	"mlx-vlm": {
		GRPCMethods:      []GRPCMethod{MethodPredict, MethodPredictStream, MethodEmbedding},
		PossibleUsecases: []string{"chat", "completion", "embeddings", "vision"},
		DefaultUsecases:  []string{"chat", "vision"},
		AcceptsImages:    true,
		AcceptsAudios:    true,
		Description:      "MLX vision-language models with multimodal input",
	},
	"mlx-audio": {
		GRPCMethods:      []GRPCMethod{MethodPredict, MethodTTS},
		PossibleUsecases: []string{"chat", "completion", "tts"},
		DefaultUsecases:  []string{"chat"},
		Description:      "MLX audio models — text generation and TTS",
	},

	// --- Image/video generation backends ---
	"diffusers": {
		GRPCMethods:      []GRPCMethod{MethodGenerateImage, MethodGenerateVideo},
		PossibleUsecases: []string{"image", "video"},
		DefaultUsecases:  []string{"image"},
		Description:      "HuggingFace diffusers — Stable Diffusion, Flux, video generation",
	},
	"stablediffusion": {
		GRPCMethods:      []GRPCMethod{MethodGenerateImage},
		PossibleUsecases: []string{"image"},
		DefaultUsecases:  []string{"image"},
		Description:      "Stable Diffusion native backend",
	},
	"stablediffusion-ggml": {
		GRPCMethods:      []GRPCMethod{MethodGenerateImage},
		PossibleUsecases: []string{"image"},
		DefaultUsecases:  []string{"image"},
		Description:      "Stable Diffusion via GGML quantized models",
	},

	// --- Speech-to-text backends ---
	"whisper": {
		GRPCMethods:      []GRPCMethod{MethodAudioTranscription, MethodVAD},
		PossibleUsecases: []string{"transcript", "vad"},
		DefaultUsecases:  []string{"transcript"},
		Description:      "OpenAI Whisper — speech recognition and voice activity detection",
	},
	"faster-whisper": {
		GRPCMethods:      []GRPCMethod{MethodAudioTranscription},
		PossibleUsecases: []string{"transcript"},
		DefaultUsecases:  []string{"transcript"},
		Description:      "CTranslate2-accelerated Whisper for faster transcription",
	},
	"whisperx": {
		GRPCMethods:      []GRPCMethod{MethodAudioTranscription},
		PossibleUsecases: []string{"transcript"},
		DefaultUsecases:  []string{"transcript"},
		Description:      "WhisperX — Whisper with word-level timestamps and speaker diarization",
	},
	"moonshine": {
		GRPCMethods:      []GRPCMethod{MethodAudioTranscription},
		PossibleUsecases: []string{"transcript"},
		DefaultUsecases:  []string{"transcript"},
		Description:      "Moonshine speech recognition",
	},
	"nemo": {
		GRPCMethods:      []GRPCMethod{MethodAudioTranscription},
		PossibleUsecases: []string{"transcript"},
		DefaultUsecases:  []string{"transcript"},
		Description:      "NVIDIA NeMo speech recognition",
	},
	"qwen-asr": {
		GRPCMethods:      []GRPCMethod{MethodAudioTranscription},
		PossibleUsecases: []string{"transcript"},
		DefaultUsecases:  []string{"transcript"},
		Description:      "Qwen automatic speech recognition",
	},
	"voxtral": {
		GRPCMethods:      []GRPCMethod{MethodAudioTranscription},
		PossibleUsecases: []string{"transcript"},
		DefaultUsecases:  []string{"transcript"},
		Description:      "Voxtral speech recognition",
	},
	"vibevoice": {
		GRPCMethods:      []GRPCMethod{MethodAudioTranscription, MethodTTS},
		PossibleUsecases: []string{"transcript", "tts"},
		DefaultUsecases:  []string{"transcript", "tts"},
		Description:      "VibeVoice — bidirectional speech (transcription and synthesis)",
	},

	// --- TTS backends ---
	"piper": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Piper — fast neural TTS optimized for Raspberry Pi",
	},
	"kokoro": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Kokoro TTS",
	},
	"coqui": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Coqui TTS — multi-speaker neural synthesis",
	},
	"kitten-tts": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Kitten TTS",
	},
	"outetts": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "OuteTTS",
	},
	"pocket-tts": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Pocket TTS — lightweight text-to-speech",
	},
	"qwen-tts": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Qwen TTS",
	},
	"faster-qwen3-tts": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Faster Qwen3 TTS — accelerated Qwen TTS",
	},
	"fish-speech": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Fish Speech TTS",
	},
	"neutts": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "NeuTTS — neural text-to-speech",
	},
	"chatterbox": {
		GRPCMethods:      []GRPCMethod{MethodTTS},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "Chatterbox TTS",
	},
	"voxcpm": {
		GRPCMethods:      []GRPCMethod{MethodTTS, MethodTTSStream},
		PossibleUsecases: []string{"tts"},
		DefaultUsecases:  []string{"tts"},
		Description:      "VoxCPM TTS with streaming support",
	},

	// --- Sound generation backends ---
	"ace-step": {
		GRPCMethods:      []GRPCMethod{MethodTTS, MethodSoundGeneration},
		PossibleUsecases: []string{"tts", "sound_generation"},
		DefaultUsecases:  []string{"sound_generation"},
		Description:      "ACE-Step — music and sound generation",
	},
	"acestep-cpp": {
		GRPCMethods:      []GRPCMethod{MethodSoundGeneration},
		PossibleUsecases: []string{"sound_generation"},
		DefaultUsecases:  []string{"sound_generation"},
		Description:      "ACE-Step C++ — native sound generation",
	},
	"transformers-musicgen": {
		GRPCMethods:      []GRPCMethod{MethodTTS, MethodSoundGeneration},
		PossibleUsecases: []string{"tts", "sound_generation"},
		DefaultUsecases:  []string{"sound_generation"},
		Description:      "Meta MusicGen via transformers — music generation from text",
	},

	// --- Utility backends ---
	"rerankers": {
		GRPCMethods:      []GRPCMethod{MethodRerank},
		PossibleUsecases: []string{"rerank"},
		DefaultUsecases:  []string{"rerank"},
		Description:      "Cross-encoder reranking models",
	},
	"rfdetr": {
		GRPCMethods:      []GRPCMethod{MethodDetect},
		PossibleUsecases: []string{"detection"},
		DefaultUsecases:  []string{"detection"},
		Description:      "RF-DETR object detection",
	},
	"silero-vad": {
		GRPCMethods:      []GRPCMethod{MethodVAD},
		PossibleUsecases: []string{"vad"},
		DefaultUsecases:  []string{"vad"},
		Description:      "Silero VAD — voice activity detection",
	},
}

// NormalizeBackendName converts backend names to the canonical hyphenated form
// used in gallery entries (e.g., "llama.cpp" → "llama-cpp").
func NormalizeBackendName(backend string) string {
	return strings.ReplaceAll(backend, ".", "-")
}

// GetBackendCapability returns the capability info for a backend, or nil if unknown.
// Handles backend name normalization.
func GetBackendCapability(backend string) *BackendCapability {
	if cap, ok := BackendCapabilities[NormalizeBackendName(backend)]; ok {
		return &cap
	}
	return nil
}

// PossibleUsecasesForBackend returns all usecases a backend can support.
// Returns nil if the backend is unknown.
func PossibleUsecasesForBackend(backend string) []string {
	if cap := GetBackendCapability(backend); cap != nil {
		return cap.PossibleUsecases
	}
	return nil
}

// DefaultUsecasesForBackend returns the conservative default usecases.
// Returns nil if the backend is unknown.
func DefaultUsecasesForBackendCap(backend string) []string {
	if cap := GetBackendCapability(backend); cap != nil {
		return cap.DefaultUsecases
	}
	return nil
}

// IsValidUsecaseForBackend checks whether a usecase is in a backend's possible set.
// Returns true for unknown backends (permissive fallback).
func IsValidUsecaseForBackend(backend, usecase string) bool {
	cap := GetBackendCapability(backend)
	if cap == nil {
		return true // unknown backend — don't restrict
	}
	return slices.Contains(cap.PossibleUsecases, usecase)
}

// AllBackendNames returns a sorted list of all known backend names.
func AllBackendNames() []string {
	names := make([]string, 0, len(BackendCapabilities))
	for name := range BackendCapabilities {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
