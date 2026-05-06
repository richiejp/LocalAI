package router

import (
	"context"
	"strings"
	"testing"
)

func TestFeatureClassifier_RoutesShortAndLong(t *testing.T) {
	c := NewFeatureClassifier([]FeatureCandidate{
		{Label: "small", Rule: CandidateRule{MaxPromptLength: 50}},
		{Label: "large"}, // wildcard, last
	})

	short, err := c.Classify(context.Background(), Probe{Prompt: "hi"})
	if err != nil {
		t.Fatalf("short: %v", err)
	}
	if short.Label != "small" {
		t.Errorf("short prompt: got %q, want small", short.Label)
	}

	long, err := c.Classify(context.Background(), Probe{Prompt: strings.Repeat("a", 200)})
	if err != nil {
		t.Fatalf("long: %v", err)
	}
	if long.Label != "large" {
		t.Errorf("long prompt: got %q, want large", long.Label)
	}
}

func TestFeatureClassifier_RequiresCode(t *testing.T) {
	c := NewFeatureClassifier([]FeatureCandidate{
		{Label: "code", Rule: CandidateRule{RequiresCode: true}},
		{Label: "chat"}, // wildcard
	})

	withCode, err := c.Classify(context.Background(), Probe{Prompt: "fix this", HasCode: true})
	if err != nil || withCode.Label != "code" {
		t.Errorf("code prompt: got %+v err=%v", withCode, err)
	}

	plain, _ := c.Classify(context.Background(), Probe{Prompt: "say hi"})
	if plain.Label != "chat" {
		t.Errorf("plain prompt: got %q, want chat", plain.Label)
	}
}

func TestFeatureClassifier_ErrorsOnNoMatch(t *testing.T) {
	// All candidates declare predicates — a probe that matches none
	// produces an error so the middleware can decide between Fallback
	// and surfacing a 5xx, rather than a silent default.
	c := NewFeatureClassifier([]FeatureCandidate{
		{Label: "small", Rule: CandidateRule{MaxPromptLength: 10}},
		{Label: "code", Rule: CandidateRule{RequiresCode: true}},
	})

	_, err := c.Classify(context.Background(), Probe{Prompt: strings.Repeat("a", 100)})
	if err == nil {
		t.Errorf("expected error when no rule matches")
	}
}

func TestFeatureClassifier_PanicsOnEmptyCandidates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("expected panic on zero candidates")
		}
	}()
	_ = NewFeatureClassifier(nil)
}
