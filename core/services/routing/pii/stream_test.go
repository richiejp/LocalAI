package pii

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func newStreamRedactor(t *testing.T, ids ...string) *Redactor {
	t.Helper()
	all := DefaultPatterns()
	chosen := all
	if len(ids) > 0 {
		chosen = pick(all, ids)
	}
	patterns, err := Compile(chosen)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return NewRedactor(patterns)
}

func TestStreamFilter_MasksAcrossChunks(t *testing.T) {
	// The most important streaming test: an email split arbitrarily
	// across chunk boundaries must mask exactly the same way as one
	// arriving in a single Push.
	red := newStreamRedactor(t, "email")
	sf := NewStreamFilter(red, nil, nil, "", "")

	// "alice@example.com" (17 bytes) split between '@' and 'e'.
	out := ""
	out += sf.Push("hi alice@")
	out += sf.Push("example.com! end")
	out += sf.Drain()

	if strings.Contains(out, "alice@example.com") {
		t.Errorf("stream leaked email across chunk boundary: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:email]") {
		t.Errorf("expected mask placeholder in output, got %q", out)
	}
}

func TestStreamFilter_BlockBecomesMask(t *testing.T) {
	// api_key_prefix is block by default. In stream mode the earlier
	// chunks are already on the wire so block is impossible — the
	// filter remaps to mask while still recording action="block" so
	// the audit log keeps the original intent.
	red := newStreamRedactor(t, "api_key_prefix")
	store := NewMemoryEventStore(0)
	defer store.Close()
	sf := NewStreamFilter(red, nil, store, "corr-1", "user-1")

	out := sf.Push("here is your token: sk-abcdefghijklmnopqrstuvwxyz0123456789 done")
	out += sf.Drain()

	if strings.Contains(out, "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("block-in-stream must mask, leaked the value: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:api_key_prefix]") {
		t.Errorf("expected mask placeholder for block-in-stream, got %q", out)
	}

	events, _ := store.List(context.Background(), ListQuery{Limit: 10})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Action != ActionBlock {
		t.Errorf("audit must record original block action, got %q", events[0].Action)
	}
	if events[0].Direction != DirectionOut {
		t.Errorf("stream events must be DirectionOut, got %q", events[0].Direction)
	}
}

func TestStreamFilter_NoMatchPassthrough(t *testing.T) {
	red := newStreamRedactor(t, "email")
	sf := NewStreamFilter(red, nil, nil, "", "")
	out := sf.Push("perfectly clean text that should") + sf.Push(" pass through unchanged.") + sf.Drain()
	if out != "perfectly clean text that should pass through unchanged." {
		t.Errorf("clean stream mutated: %q", out)
	}
}

func TestStreamFilter_NilRedactorPassthrough(t *testing.T) {
	// --disable-pii path: NewStreamFilter(nil, ...) returns a filter
	// that just forwards Push input verbatim.
	sf := NewStreamFilter(nil, nil, nil, "", "")
	out := sf.Push("any old text including alice@example.com") + sf.Drain()
	if out != "any old text including alice@example.com" {
		t.Errorf("nil redactor must pass text through, got %q", out)
	}
}

func TestStreamFilter_PerModelOverrides(t *testing.T) {
	// email defaults to mask; per-model override upgrades to block.
	// In stream mode the override still maps to mask placeholder, but
	// the audit event records action="block".
	red := newStreamRedactor(t, "email")
	store := NewMemoryEventStore(0)
	defer store.Close()
	sf := NewStreamFilter(red, map[string]Action{"email": ActionBlock}, store, "corr-2", "user-2")

	out := sf.Push("contact alice@example.com please") + sf.Drain()
	if strings.Contains(out, "alice@example.com") {
		t.Errorf("override block-in-stream must mask, got %q", out)
	}
	events, _ := store.List(context.Background(), ListQuery{Limit: 10})
	if len(events) != 1 || events[0].Action != ActionBlock {
		t.Errorf("expected one block event, got %+v", events)
	}
}

// TestStreamFilter_BufferedEmitInvariant feeds the redactor a corpus
// one rune at a time, randomly chunked, and asserts:
//
//   1. Across all (input, splitting) pairs, the cumulative emitted
//      output never contains any of the secret values that were
//      embedded in the input.
//   2. The output, fully drained, equals what Redact would have
//      produced on the unsplit input.
//
// This is the load-bearing property of streaming PII: regardless of
// where chunks split, the emitted bytes cannot contain a value that a
// single-shot redactor would have masked.
func TestStreamFilter_BufferedEmitInvariant(t *testing.T) {
	corpus := []struct {
		text    string
		secrets []string
	}{
		{"contact alice@example.com or bob@example.org", []string{"alice@example.com", "bob@example.org"}},
		{"my SSN is 123-45-6789 and his is 987-65-4321", []string{"123-45-6789", "987-65-4321"}},
		{"sk-abcdefghijklmnopqrstuvwxyz0123456789 leaked", []string{"sk-abcdefghijklmnopqrstuvwxyz0123456789"}},
		{"repeats: alice@example.com / alice@example.com / alice@example.com", []string{"alice@example.com"}},
	}

	red := newStreamRedactor(t) // all default patterns
	rng := rand.New(rand.NewSource(1)) // seeded for reproducibility

	for _, tc := range corpus {
		for trial := 0; trial < 10; trial++ {
			sf := NewStreamFilter(red, nil, nil, "", "")
			var out strings.Builder
			for i := 0; i < utf8.RuneCountInString(tc.text); {
				// Random chunk size 1-8 runes, never crossing the end.
				chunk := 1 + rng.Intn(8)
				if i+chunk > utf8.RuneCountInString(tc.text) {
					chunk = utf8.RuneCountInString(tc.text) - i
				}
				out.WriteString(sf.Push(stringSlice(tc.text, i, i+chunk)))
				i += chunk
			}
			out.WriteString(sf.Drain())
			result := out.String()

			// Property 1: no secret value appears anywhere in the
			// output.
			for _, secret := range tc.secrets {
				if strings.Contains(result, secret) {
					t.Errorf("trial %d: secret %q leaked through streaming\n  input: %q\n  output: %q", trial, secret, tc.text, result)
				}
			}

			// Property 2: the streamed output equals what a single-shot
			// Redact would have produced on the same input. (Block
			// patterns get masked in stream mode, so we compare against
			// a remapped redaction.)
			expected := singleShotMaskAll(red, tc.text)
			if result != expected {
				t.Errorf("trial %d: stream != single-shot\n  input: %q\n  stream: %q\n  expected: %q",
					trial, tc.text, result, expected)
			}
		}
	}
}

// singleShotMaskAll runs the redactor in one pass with all blocks
// remapped to mask — the same view the StreamFilter produces.
func singleShotMaskAll(red *Redactor, text string) string {
	patterns := red.Patterns()
	overrides := make(map[string]Action, len(patterns))
	for _, p := range patterns {
		if p.Action == ActionBlock {
			overrides[p.ID] = ActionMask
		}
	}
	res := red.RedactWithOverrides(text, overrides)
	return res.Redacted
}

func stringSlice(s string, fromRune, toRune int) string {
	runes := []rune(s)
	return string(runes[fromRune:toRune])
}
