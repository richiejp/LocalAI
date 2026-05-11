package router

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubLLM returns canned responses based on substring match in the
// user prompt. Records every system+user pair so tests can verify
// the system prompt encodes labels + descriptions.
type stubLLM struct {
	responses map[string]string
	calls     int
	lastSys   string
	lastUser  string
	failNext  bool
}

func (s *stubLLM) Complete(_ context.Context, system, user string) (string, error) {
	s.calls++
	s.lastSys = system
	s.lastUser = user
	if s.failNext {
		s.failNext = false
		return "", errors.New("llm: backend unavailable")
	}
	for needle, resp := range s.responses {
		if strings.Contains(strings.ToLower(user), needle) {
			return resp, nil
		}
	}
	return "unknown", nil
}

func TestLLMClassifier_PicksLabelFromCleanResponse(t *testing.T) {
	llm := &stubLLM{responses: map[string]string{"code": "code", "weather": "weather"}}
	c := NewLLMClassifier(
		[]LLMCandidate{
			{Label: "code", Description: "programming questions"},
			{Label: "weather", Description: "weather questions"},
		},
		llm,
		0,
	)
	d, err := c.Classify(context.Background(), Probe{Prompt: "show me the code"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.Label != "code" {
		t.Errorf("Label = %q, want code", d.Label)
	}
}

func TestLLMClassifier_TolerantOfChattyResponse(t *testing.T) {
	// Small instruct models often add preamble despite the system
	// prompt asking for label-only. Parser must extract the label
	// from a substring rather than insisting on exact match.
	llm := &stubLLM{responses: map[string]string{"weather": "I'd pick: weather"}}
	c := NewLLMClassifier(
		[]LLMCandidate{
			{Label: "code", Description: "programming"},
			{Label: "weather", Description: "weather"},
		},
		llm,
		0,
	)
	d, err := c.Classify(context.Background(), Probe{Prompt: "weather please"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.Label != "weather" {
		t.Errorf("Label = %q, want weather (parser must scan past preamble)", d.Label)
	}
}

func TestLLMClassifier_OverlappingLabelsPickLongestFirst(t *testing.T) {
	// "code" is a substring of "code-review"; map iteration order is
	// randomised, so a naive substring scan would non-deterministically
	// pick "code" half the time even when the LLM said "code-review".
	// parseLabel sorts labels longest-first to make the choice
	// deterministic.
	llm := &stubLLM{responses: map[string]string{"review": "code-review"}}
	c := NewLLMClassifier(
		[]LLMCandidate{
			{Label: "code", Description: "general programming"},
			{Label: "code-review", Description: "review my code"},
		},
		llm,
		0,
	)
	d, err := c.Classify(context.Background(), Probe{Prompt: "review please"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.Label != "code-review" {
		t.Errorf("Label = %q, want code-review (longest-match-first)", d.Label)
	}
}

func TestLLMClassifier_HallucinatedLabelIsRejected(t *testing.T) {
	llm := &stubLLM{responses: map[string]string{"anything": "math"}}
	c := NewLLMClassifier(
		[]LLMCandidate{
			{Label: "code", Description: "programming"},
			{Label: "weather", Description: "weather"},
		},
		llm,
		0,
	)
	_, err := c.Classify(context.Background(), Probe{Prompt: "anything please"})
	if err == nil {
		t.Error("expected error for hallucinated label")
	}
}

func TestLLMClassifier_CacheSkipsRepeatLLMCall(t *testing.T) {
	llm := &stubLLM{responses: map[string]string{"code": "code"}}
	c := NewLLMClassifier(
		[]LLMCandidate{
			{Label: "code", Description: "programming"},
			{Label: "weather", Description: "weather"},
		},
		llm,
		16,
	)
	if _, err := c.Classify(context.Background(), Probe{Prompt: "code one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Classify(context.Background(), Probe{Prompt: "code one"}); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 {
		t.Errorf("expected 1 LLM call (second hit cache), got %d", llm.calls)
	}
}

func TestLLMClassifier_CacheCapEvicts(t *testing.T) {
	llm := &stubLLM{responses: map[string]string{"x": "code"}}
	c := NewLLMClassifier(
		[]LLMCandidate{{Label: "code", Description: "programming"}},
		llm,
		2,
	)
	for i := 0; i < 5; i++ {
		// Each prompt is unique so the cache fills then evicts.
		_, err := c.Classify(context.Background(), Probe{Prompt: "x" + string(rune('a'+i))})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := c.CacheLen(); got > 2 {
		t.Errorf("cache exceeded cap: len=%d cap=2", got)
	}
}

func TestLLMClassifier_SystemPromptListsAllCandidates(t *testing.T) {
	// The system prompt is the LLM's only source of truth for what
	// labels it can return. Verify every candidate's label and
	// description appear so a missing description doesn't silently
	// produce no-match responses.
	llm := &stubLLM{responses: map[string]string{"a": "code"}}
	c := NewLLMClassifier(
		[]LLMCandidate{
			{Label: "code", Description: "programming questions"},
			{Label: "math", Description: "math problems"},
		},
		llm,
		0,
	)
	if _, err := c.Classify(context.Background(), Probe{Prompt: "alpha"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"code", "math", "programming questions", "math problems"} {
		if !strings.Contains(llm.lastSys, want) {
			t.Errorf("system prompt missing %q\n%s", want, llm.lastSys)
		}
	}
}

func TestLLMClassifier_LLMFailureSurfacesAsError(t *testing.T) {
	llm := &stubLLM{failNext: true}
	c := NewLLMClassifier(
		[]LLMCandidate{{Label: "code", Description: "programming"}},
		llm,
		0,
	)
	_, err := c.Classify(context.Background(), Probe{Prompt: "anything"})
	if err == nil {
		t.Error("expected llm failure to surface")
	}
}

func TestLLMClassifier_PanicsOnEmptyDescription(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for empty description")
		}
	}()
	NewLLMClassifier(
		[]LLMCandidate{{Label: "code", Description: ""}},
		&stubLLM{},
		0,
	)
}
