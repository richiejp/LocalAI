package routes

import (
	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/application"
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/http/endpoints/localai"
	"github.com/mudler/LocalAI/core/http/endpoints/openai"
	"github.com/mudler/LocalAI/core/http/middleware"
	"github.com/mudler/LocalAI/core/schema"
)

func RegisterOpenAIRoutes(app *echo.Echo,
	re *middleware.RequestExtractor,
	application *application.Application) {
	// openAI compatible API endpoint
	traceMiddleware := middleware.TraceMiddleware(application)

	// realtime
	// TODO: Modify/disable the API key middleware for this endpoint to allow ephemeral keys created by sessions
	app.GET("/v1/realtime", openai.Realtime(application))
	app.POST("/v1/realtime/sessions", openai.RealtimeTranscriptionSession(application), traceMiddleware)
	app.POST("/v1/realtime/transcription_session", openai.RealtimeTranscriptionSession(application), traceMiddleware)

	// chat
	chatHandler := openai.ChatEndpoint(application.ModelLoader(), application.TemplatesEvaluator())
	chatMiddleware := []echo.MiddlewareFunc{
		traceMiddleware,
		re.BuildFilteredFirstAvailableDefaultModel(config.BuildUsecaseFilterFn(config.FLAG_CHAT)),
		re.SetModelAndConfig(func() schema.LocalAIRequest { return new(schema.OpenAIRequest) }),
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if err := re.SetOpenAIRequest(c); err != nil {
					return err
				}
				return next(c)
			}
		},
	}
	app.POST("/v1/chat/completions", chatHandler, chatMiddleware...)
	app.POST("/chat/completions", chatHandler, chatMiddleware...)

	// edit
	editHandler := openai.EditEndpoint(application.ModelLoader(), application.TemplatesEvaluator())
	editMiddleware := []echo.MiddlewareFunc{
		traceMiddleware,
		re.BuildFilteredFirstAvailableDefaultModel(config.BuildUsecaseFilterFn(config.FLAG_EDIT)),
		re.BuildConstantDefaultModelNameMiddleware("gpt-4o"),
		re.SetModelAndConfig(func() schema.LocalAIRequest { return new(schema.OpenAIRequest) }),
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if err := re.SetOpenAIRequest(c); err != nil {
					return err
				}
				return next(c)
			}
		},
	}
	app.POST("/v1/edits", editHandler, editMiddleware...)
	app.POST("/edits", editHandler, editMiddleware...)

	// completion
	completionHandler := openai.CompletionEndpoint(application.ModelLoader(), application.TemplatesEvaluator())
	completionMiddleware := []echo.MiddlewareFunc{
		traceMiddleware,
		re.BuildFilteredFirstAvailableDefaultModel(config.BuildUsecaseFilterFn(config.FLAG_COMPLETION)),
		re.BuildConstantDefaultModelNameMiddleware("gpt-4o"),
		re.SetModelAndConfig(func() schema.LocalAIRequest { return new(schema.OpenAIRequest) }),
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if err := re.SetOpenAIRequest(c); err != nil {
					return err
				}
				return next(c)
			}
		},
	}
	app.POST("/v1/completions", completionHandler, completionMiddleware...)
	app.POST("/completions", completionHandler, completionMiddleware...)
	app.POST("/v1/engines/:model/completions", completionHandler, completionMiddleware...)

	// embeddings
	embeddingHandler := openai.EmbeddingsEndpoint(application.ModelLoader())
	embeddingMiddleware := []echo.MiddlewareFunc{
		traceMiddleware,
		re.BuildFilteredFirstAvailableDefaultModel(config.BuildUsecaseFilterFn(config.FLAG_EMBEDDINGS)),
		re.BuildConstantDefaultModelNameMiddleware("gpt-4o"),
		re.SetModelAndConfig(func() schema.LocalAIRequest { return new(schema.OpenAIRequest) }),
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if err := re.SetOpenAIRequest(c); err != nil {
					return err
				}
				return next(c)
			}
		},
	}
	app.POST("/v1/embeddings", embeddingHandler, embeddingMiddleware...)
	app.POST("/embeddings", embeddingHandler, embeddingMiddleware...)
	app.POST("/v1/engines/:model/embeddings", embeddingHandler, embeddingMiddleware...)

	audioHandler := openai.TranscriptEndpoint(application.ModelLoader())
	audioMiddleware := []echo.MiddlewareFunc{
		traceMiddleware,
		re.BuildFilteredFirstAvailableDefaultModel(config.BuildUsecaseFilterFn(config.FLAG_TRANSCRIPT)),
		re.SetModelAndConfig(func() schema.LocalAIRequest { return new(schema.OpenAIRequest) }),
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if err := re.SetOpenAIRequest(c); err != nil {
					return err
				}
				return next(c)
			}
		},
	}
	// audio
	app.POST("/v1/audio/transcriptions", audioHandler, audioMiddleware...)
	app.POST("/audio/transcriptions", audioHandler, audioMiddleware...)

	audioSpeechHandler := localai.TTSEndpoint(application.ModelLoader())
	audioSpeechMiddleware := []echo.MiddlewareFunc{
		traceMiddleware,
		re.BuildFilteredFirstAvailableDefaultModel(config.BuildUsecaseFilterFn(config.FLAG_TTS)),
		re.SetModelAndConfig(func() schema.LocalAIRequest { return new(schema.TTSRequest) }),
	}

	app.POST("/v1/audio/speech", audioSpeechHandler, audioSpeechMiddleware...)
	app.POST("/audio/speech", audioSpeechHandler, audioSpeechMiddleware...)

	// images
	imageHandler := openai.ImageEndpoint(application.ModelLoader())
	imageMiddleware := []echo.MiddlewareFunc{
		traceMiddleware,
		// Default: use the first available image generation model
		re.BuildFilteredFirstAvailableDefaultModel(config.BuildUsecaseFilterFn(config.FLAG_IMAGE)),
		re.SetModelAndConfig(func() schema.LocalAIRequest { return new(schema.OpenAIRequest) }),
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if err := re.SetOpenAIRequest(c); err != nil {
					return err
				}
				return next(c)
			}
		},
	}

	app.POST("/v1/images/generations", imageHandler, imageMiddleware...)
	app.POST("/images/generations", imageHandler, imageMiddleware...)

	// inpainting endpoint (image + mask) - reuse same middleware config as images
	inpaintingHandler := openai.InpaintingEndpoint(application.ModelLoader())
	app.POST("/v1/images/inpainting", inpaintingHandler, imageMiddleware...)
	app.POST("/images/inpainting", inpaintingHandler, imageMiddleware...)

	// videos (OpenAI-compatible endpoints mapped to LocalAI video handler)
	videoHandler := openai.VideoEndpoint(application.ModelLoader())
	videoMiddleware := []echo.MiddlewareFunc{
		traceMiddleware,
		re.BuildFilteredFirstAvailableDefaultModel(config.BuildUsecaseFilterFn(config.FLAG_VIDEO)),
		re.SetModelAndConfig(func() schema.LocalAIRequest { return new(schema.OpenAIRequest) }),
		func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				if err := re.SetOpenAIRequest(c); err != nil {
					return err
				}
				return next(c)
			}
		},
	}

	// OpenAI-style create video endpoint
	app.POST("/v1/videos", videoHandler, videoMiddleware...)
	app.POST("/v1/videos/generations", videoHandler, videoMiddleware...)
	app.POST("/videos", videoHandler, videoMiddleware...)

	// List models
	app.GET("/v1/models", openai.ListModelsEndpoint(application.ModelLoader()))
	app.GET("/models", openai.ListModelsEndpoint(application.ModelLoader()))
}
