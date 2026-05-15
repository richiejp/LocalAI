package router

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// CandidateScore mirrors core/backend.CandidateScore at the router-
// boundary. Lives here so the router can depend on this abstraction
// without importing core/backend.
type CandidateScore struct {
	LogProb                 float64
	LengthNormalizedLogProb float64
	NumTokens               int
}

// Scorer evaluates a model's joint log-probability of each candidate
// continuation given a shared prompt. The classifier consumes this for
// multi-label policy selection (read off the distribution rather than
// asking the model to emit a single argmax token).
type Scorer interface {
	Score(ctx context.Context, prompt string, candidates []string) ([]CandidateScore, error)
}

// ScorePolicy mirrors config.RouterPolicy at the classifier boundary —
// a label string plus its natural-language description for the
// routing system prompt.
type ScorePolicy struct {
	Label       string
	Description string
}

// defaultActivationThreshold is the softmax-probability floor a policy
// must clear to be considered "active." Picked low enough that two
// reasonably-confident labels (each ~0.4) both activate, high enough
// that a flat distribution doesn't activate everything.
const defaultActivationThreshold = 0.15

// ScoreClassifier scores every policy label as a continuation of the
// routing prompt, converts log-probabilities into a softmax
// distribution, and returns the set of labels whose probability
// passes the activation threshold.
//
// This is the off-the-shelf-Arch-Router approach extended for multi-
// label. The classifier model is trained to emit a single policy
// label, but its output distribution still spreads probability mass
// across competing labels when more than one applies. Reading the
// distribution rather than the argmax lets us route conjunctive
// intents ("debug this code AND explain the math") to a candidate
// that can serve both.
type ScoreClassifier struct {
	policies            []ScorePolicy
	scorer              Scorer
	activationThreshold float64

	// systemPrompt is built once at construction. The same prompt is
	// reused on every classification — only the user-turn body changes.
	systemPrompt string

	// labels stored in the order configured. Indices into policies[]
	// align with the scorer's input/output ordering.
	labelOrder []string

	cache *labelSetCache
}

// NewScoreClassifier panics on caller errors at construction (empty
// policies, missing description, nil scorer) — same rationale as the
// other classifiers. cacheCap=0 disables the cache.
// activationThreshold=0 picks the package default (0.15).
func NewScoreClassifier(policies []ScorePolicy, scorer Scorer, cacheCap int, activationThreshold float64) *ScoreClassifier {
	if len(policies) == 0 {
		panic("router/score: at least one policy is required")
	}
	if scorer == nil {
		panic("router/score: scorer is required (configure router.classifier_model)")
	}
	for _, p := range policies {
		if p.Label == "" {
			panic("router/score: policy has empty label")
		}
		if p.Description == "" {
			panic(fmt.Sprintf("router/score: policy %q has no description", p.Label))
		}
	}
	labels := make([]string, 0, len(policies))
	for _, p := range policies {
		labels = append(labels, p.Label)
	}
	if activationThreshold <= 0 {
		activationThreshold = defaultActivationThreshold
	}
	return &ScoreClassifier{
		policies:            policies,
		scorer:              scorer,
		activationThreshold: activationThreshold,
		systemPrompt:        buildScoreSystemPrompt(policies),
		labelOrder:          labels,
		cache:               newLabelSetCache(cacheCap),
	}
}

func (c *ScoreClassifier) Name() string { return ClassifierScore }

func (c *ScoreClassifier) Classify(ctx context.Context, p Probe) (Decision, error) {
	start := time.Now()
	if hit, ok := c.cache.lookup(p.Prompt); ok {
		return Decision{Labels: hit, Score: 1.0, Latency: time.Since(start)}, nil
	}
	prompt := buildScorePrompt(c.systemPrompt, p.Prompt)
	results, err := c.scorer.Score(ctx, prompt, c.labelOrder)
	if err != nil {
		return errDecision(start, fmt.Errorf("score classify: %w", err))
	}
	if len(results) != len(c.labelOrder) {
		return errDecision(start, fmt.Errorf("score classify: scorer returned %d results for %d policies", len(results), len(c.labelOrder)))
	}

	// Convert per-token log-probabilities into a probability
	// distribution over labels via softmax. Length-normalisation
	// makes labels of different token lengths comparable, then
	// softmax converts to probabilities suitable for thresholding.
	logProbs := make([]float64, len(results))
	for i, r := range results {
		switch {
		case r.NumTokens == 0:
			logProbs[i] = math.Inf(-1)
		case r.LengthNormalizedLogProb != 0:
			logProbs[i] = r.LengthNormalizedLogProb
		default:
			// Backend didn't populate the length-normalised field;
			// derive it ourselves so candidates of unequal token
			// length stay comparable.
			logProbs[i] = r.LogProb / float64(r.NumTokens)
		}
	}
	probs := softmax(logProbs)

	// Threshold to active label set. Top probability is also kept
	// for the decision-log "score" field so admins can see
	// confidence even when multiple labels are active.
	active := make([]string, 0, len(c.labelOrder))
	topProb := 0.0
	for i, prob := range probs {
		if prob > topProb {
			topProb = prob
		}
		if prob >= c.activationThreshold {
			active = append(active, c.labelOrder[i])
		}
	}
	// Defensive: if the distribution is so flat that nothing
	// crosses threshold, fall back to the argmax so the caller
	// always has something to route on.
	if len(active) == 0 {
		bestIdx := 0
		for i := 1; i < len(probs); i++ {
			if probs[i] > probs[bestIdx] {
				bestIdx = i
			}
		}
		active = []string{c.labelOrder[bestIdx]}
	}
	c.cache.put(p.Prompt, active)
	return Decision{
		Labels:  active,
		Score:   topProb,
		Latency: time.Since(start),
	}, nil
}

// softmax converts an array of log-probabilities into a probability
// distribution. -inf inputs are handled (their exp contributes 0).
// Uses the standard max-subtraction trick for numerical stability.
func softmax(logProbs []float64) []float64 {
	if len(logProbs) == 0 {
		return nil
	}
	maxLP := math.Inf(-1)
	for _, lp := range logProbs {
		if lp > maxLP {
			maxLP = lp
		}
	}
	if math.IsInf(maxLP, -1) {
		// All -inf: return a uniform distribution as a sensible
		// degenerate result.
		out := make([]float64, len(logProbs))
		for i := range out {
			out[i] = 1.0 / float64(len(logProbs))
		}
		return out
	}
	out := make([]float64, len(logProbs))
	sum := 0.0
	for i, lp := range logProbs {
		out[i] = math.Exp(lp - maxLP)
		sum += out[i]
	}
	if sum == 0 {
		// Shouldn't happen given the maxLP check above, but guard
		// against pathological inputs.
		for i := range out {
			out[i] = 1.0 / float64(len(out))
		}
		return out
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}

// CacheLen returns the number of cached prompts. Test-only API.
func (c *ScoreClassifier) CacheLen() int { return c.cache.count() }

func buildScoreSystemPrompt(policies []ScorePolicy) string {
	var b strings.Builder
	b.WriteString("You are a routing classifier. Pick the policy whose description best matches the user's request.\n\n")
	b.WriteString("Available policies:\n")
	for _, p := range policies {
		b.WriteString("- ")
		b.WriteString(p.Label)
		b.WriteString(": ")
		b.WriteString(p.Description)
		b.WriteString("\n")
	}
	return b.String()
}

// buildScorePrompt assembles the Qwen/ChatML-style prompt the
// Arch-Router model was trained on. The candidate label is scored as
// the assistant's first token(s) of response — so we end the prompt
// right at the assistant-turn marker, no trailing newline.
//
// Hard-coded to ChatML for now: Arch-Router is Qwen-2.5-1.5B-Instruct
// based and the published GGUF carries this template natively. When
// we add a non-ChatML scoring model we'll thread the template through
// from ModelConfig.
func buildScorePrompt(system, user string) string {
	var b strings.Builder
	b.WriteString("<|im_start|>system\n")
	b.WriteString(system)
	b.WriteString("<|im_end|>\n<|im_start|>user\n")
	b.WriteString(user)
	b.WriteString("<|im_end|>\n<|im_start|>assistant\n")
	return b.String()
}
