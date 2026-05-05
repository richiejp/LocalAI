package pii

import (
	"strings"
	"testing"
)

func mustCompile(t *testing.T, ids ...string) []Pattern {
	t.Helper()
	all := DefaultPatterns()
	if len(ids) == 0 {
		out, err := Compile(all)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return out
	}
	pick := pick(all, ids)
	out, err := Compile(pick)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return out
}

func pick(all []Pattern, ids []string) []Pattern {
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	var out []Pattern
	for _, p := range all {
		if keep[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

func TestRedactEmail(t *testing.T) {
	r := NewRedactor(mustCompile(t, "email"))
	res := r.Redact("Contact me at alice@example.com any time.")
	if res.Blocked {
		t.Fatalf("email is mask-action by default, should not block")
	}
	if !strings.Contains(res.Redacted, "[REDACTED:email]") {
		t.Errorf("expected mask placeholder, got %q", res.Redacted)
	}
	if strings.Contains(res.Redacted, "alice@example.com") {
		t.Errorf("redacted output still contains the email: %q", res.Redacted)
	}
	if len(res.Spans) != 1 {
		t.Errorf("expected 1 span, got %d", len(res.Spans))
	}
	if res.Spans[0].HashPrefix == "" {
		t.Errorf("hash prefix must be set so audits can dedupe leaks")
	}
}

func TestRedactSSN(t *testing.T) {
	r := NewRedactor(mustCompile(t, "ssn"))
	res := r.Redact("call me about SSN 123-45-6789 please")
	if !strings.Contains(res.Redacted, "[REDACTED:ssn]") {
		t.Errorf("ssn not redacted: %q", res.Redacted)
	}
}

func TestRedactCreditCardLuhn(t *testing.T) {
	r := NewRedactor(mustCompile(t, "credit_card"))

	// 4111 1111 1111 1111 — canonical Luhn-valid Visa test number.
	good := r.Redact("card: 4111 1111 1111 1111")
	if len(good.Spans) != 1 || !strings.Contains(good.Redacted, "[REDACTED:credit_card]") {
		t.Errorf("Luhn-valid card should be redacted, got %+v / %q", good.Spans, good.Redacted)
	}

	// 4111 1111 1111 1112 — same shape, fails Luhn. Must NOT match.
	bad := r.Redact("card: 4111 1111 1111 1112")
	if len(bad.Spans) != 0 {
		t.Errorf("Luhn-invalid 16-digit run must not be redacted, got %+v", bad.Spans)
	}
	if !strings.Contains(bad.Redacted, "1112") {
		t.Errorf("Luhn-invalid input should pass through untouched: %q", bad.Redacted)
	}
}

func TestRedactIPv4OctetCheck(t *testing.T) {
	r := NewRedactor(mustCompile(t, "ipv4"))

	good := r.Redact("server at 192.168.1.10 is up")
	if len(good.Spans) != 1 {
		t.Errorf("valid ipv4 should redact: %+v", good.Spans)
	}

	// 999.999.999.999 — regex matches but octet > 255 must reject.
	bad := r.Redact("not an ip: 999.999.999.999")
	if len(bad.Spans) != 0 {
		t.Errorf("ipv4 with octet>255 must not match, got %+v", bad.Spans)
	}
}

func TestApiKeyDefaultsToBlock(t *testing.T) {
	r := NewRedactor(mustCompile(t, "api_key_prefix"))
	res := r.Redact("here's a token sk-abcdefghijklmnopqrstuvwxyz0123456789 to use")
	if !res.Blocked {
		t.Errorf("api_key default action is block; Result.Blocked must be true. Spans=%+v", res.Spans)
	}
	// The redacted output keeps the matched value when blocking — the
	// caller is expected to refuse the request, not to forward a partial.
	if !strings.Contains(res.Redacted, "sk-abcdefghijklmn") {
		t.Errorf("blocked actions leave the matched span intact for caller inspection: %q", res.Redacted)
	}
}

func TestRedactPreservesNonMatchingText(t *testing.T) {
	r := NewRedactor(mustCompile(t)) // all default patterns
	in := "no PII here at all, just words and numbers like 42 and 1.5"
	res := r.Redact(in)
	if res.Redacted != in {
		t.Errorf("non-PII input should pass through unchanged.\nin:  %q\nout: %q", in, res.Redacted)
	}
	if len(res.Spans) != 0 {
		t.Errorf("expected 0 spans on non-PII input, got %+v", res.Spans)
	}
}

func TestRedactEmptyInput(t *testing.T) {
	r := NewRedactor(mustCompile(t))
	res := r.Redact("")
	if res.Redacted != "" || res.Blocked || res.LocalOnly || len(res.Spans) != 0 {
		t.Errorf("empty input should yield empty result, got %+v", res)
	}
}

func TestRedactNilPatterns(t *testing.T) {
	// Disabled-PII deployment: pii.NewRedactor(nil) is a no-op.
	r := NewRedactor(nil)
	res := r.Redact("alice@example.com sent it")
	if res.Redacted != "alice@example.com sent it" {
		t.Errorf("nil patterns must be a no-op, got %q", res.Redacted)
	}
}

func TestHashPrefixStability(t *testing.T) {
	r := NewRedactor(mustCompile(t, "email"))
	a := r.Redact("a@b.com")
	b := r.Redact("hi a@b.com again")
	if len(a.Spans) != 1 || len(b.Spans) != 1 {
		t.Fatalf("unexpected span counts: %d, %d", len(a.Spans), len(b.Spans))
	}
	if a.Spans[0].HashPrefix != b.Spans[0].HashPrefix {
		t.Errorf("same matched value must produce same hash prefix: %q vs %q",
			a.Spans[0].HashPrefix, b.Spans[0].HashPrefix)
	}
}

func TestCompileRejectsUnknownPatternID(t *testing.T) {
	_, err := Compile([]Pattern{{ID: "nonexistent", Action: ActionMask}})
	if err == nil {
		t.Fatal("Compile must error on unknown pattern id; got nil")
	}
}

func TestMaxPatternLength(t *testing.T) {
	patterns := mustCompile(t, "email", "ssn")
	got := MaxPatternLength(patterns)
	// email is the longer of the two (254). The streaming filter
	// will use this to size its tail buffer.
	if got != 254 {
		t.Errorf("MaxPatternLength: got %d, want 254", got)
	}
}
