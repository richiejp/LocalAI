package router

import (
	"context"
	"fmt"
	"time"
)

// CandidateRule is the duck-typed view of config.RouterCandidateRule
// the feature classifier needs. The router package is core/config-free
// (avoids a cycle through config → router → config), so callers
// translate their RouterCandidate slice into FeatureCandidate slices
// at construction time.
type CandidateRule struct {
	MaxPromptLength int
	MinPromptLength int
	RequiresCode    bool
}

// FeatureCandidate pairs a label with its rule. Order matters: the
// first rule whose predicates all match wins. A candidate with no
// rule fields populated acts as a wildcard — callers should put the
// wildcard last so the more-specific rules get a chance to match.
type FeatureCandidate struct {
	Label string
	Rule  CandidateRule
}

// FeatureClassifier picks a label using handcrafted rules over the
// probe's length and content shape. Deterministic and table-driven —
// the natural MVP for the routing module before the KNN tier lands.
type FeatureClassifier struct {
	candidates []FeatureCandidate
}

// NewFeatureClassifier panics on an empty candidate slice. That's
// caller error — a router with no candidates is meaningless and the
// surrounding middleware refuses to engage one. We panic rather than
// return an error because the construction site is in startup wiring,
// not request hot path; the panic surfaces config bugs early.
func NewFeatureClassifier(candidates []FeatureCandidate) *FeatureClassifier {
	if len(candidates) == 0 {
		panic("router/feature: at least one candidate is required")
	}
	return &FeatureClassifier{candidates: candidates}
}

func (f *FeatureClassifier) Name() string { return "feature" }

func (f *FeatureClassifier) Classify(_ context.Context, p Probe) (Decision, error) {
	start := time.Now()
	for _, c := range f.candidates {
		if matches(c.Rule, p) {
			return Decision{
				Label:   c.Label,
				Score:   1.0,
				Latency: time.Since(start),
			}, nil
		}
	}
	// Every match path hit a rule. We surface this as an error so the
	// middleware can fall through to the configured Fallback or fail
	// the request — silently picking a candidate would be the wrong
	// default.
	return Decision{Latency: time.Since(start)}, fmt.Errorf("no candidate rule matched")
}

func matches(rule CandidateRule, p Probe) bool {
	if rule.MaxPromptLength > 0 && len(p.Prompt) > rule.MaxPromptLength {
		return false
	}
	if rule.MinPromptLength > 0 && len(p.Prompt) < rule.MinPromptLength {
		return false
	}
	if rule.RequiresCode && !p.HasCode {
		return false
	}
	// All explicit predicates passed. A candidate with no predicates
	// (the zero rule) is a wildcard that always matches — the
	// expected "fallback last" pattern.
	return true
}
