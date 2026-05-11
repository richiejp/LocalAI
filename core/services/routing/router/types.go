// Package router holds the routing module's classifier interface and
// rule/feature/knn/llm implementations.
//
// The dispatch architecture is: a "router model" in ModelConfig (one
// with a Router block) gets matched at request time. The classifier
// inspects the prompt and picks one of the candidate labels; the
// surrounding middleware rewrites input.Model to the matched
// candidate's model and falls back through the existing model
// resolution path. This keeps ACL checks, disabled-state, and per-
// model PII consistent — the router does *model* selection, nothing
// else.
//
// The package deliberately has no dependency on core/http or
// core/services — those wire the classifier in and feed it the request
// shape they own. Keeps the classifier easy to unit-test against
// synthetic Probe inputs and reusable from non-HTTP entry points
// (e.g., a future MCP routing tool).
package router

import (
	"context"
	"time"
)

// Probe is the classifier's input — the parsed prompt content the
// classifier needs to make a decision. Fields are populated by the
// caller (the middleware does the schema-shape extraction); the
// classifier never inspects the original request struct.
//
// Concrete classifiers may inspect any subset; the feature classifier
// reads Prompt and HasCode, knn would embed Prompt, llm would feed
// Prompt to a small model.
type Probe struct {
	// Prompt is the merged user-visible text. For chat completions it
	// is the concatenation of message contents (separated by newlines);
	// for plain completions it is the raw prompt.
	Prompt string

	// HasCode is true when the prompt contains a triple-backtick fence
	// or another strong code marker. The middleware computes it once
	// so every classifier sees the same signal.
	HasCode bool
}

// Decision is the classifier's output. Label is the candidate label
// the caller looks up in the Router config. Score is classifier-
// specific (rule-based: 1.0 always; knn: cosine similarity; llm:
// log-prob); kept for the decision log so admins can spot uncertain
// choices.
type Decision struct {
	Label   string        `json:"label"`
	Score   float64       `json:"score"`
	Latency time.Duration `json:"latency"`
}

// Classifier is the entry point the middleware calls. The
// implementation is responsible for honouring ctx cancellation —
// long-running classifiers (llm) must abort when the request context
// dies.
type Classifier interface {
	Classify(ctx context.Context, p Probe) (Decision, error)
	// Name is a stable identifier that ends up in RouterDecision rows
	// — admins read this to know which classifier produced a given
	// decision when more than one is configured across models.
	Name() string
}

// Classifier names. Single source of truth for the YAML
// classifier: field, the buildClassifier dispatch in the
// middleware, and the strings each Classifier returns from Name().
const (
	ClassifierFeature = "feature"
	ClassifierKNN     = "knn"
	ClassifierLLM     = "llm"
)

// LabelFallback is the synthetic label written to the decision
// store when the middleware uses cfg.Router.Fallback rather than a
// classifier-picked candidate.
const LabelFallback = "fallback"

// errDecision packages an error with a populated Latency so each
// classifier's Classify can return early without restating the
// `Decision{Latency: time.Since(start)}, err` pattern.
func errDecision(start time.Time, err error) (Decision, error) {
	return Decision{Latency: time.Since(start)}, err
}
