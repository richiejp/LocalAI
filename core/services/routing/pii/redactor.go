package pii

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Redactor scans text against a configured pattern set and applies the
// per-pattern action. The pattern set itself is mutable at runtime via
// SetAction (the /api/pii/patterns/:id admin endpoint mutates it
// in-place); reads are guarded by a mutex so concurrent requests stay
// race-free.
type Redactor struct {
	mu       sync.RWMutex
	patterns []Pattern
	maxLen   int
}

// NewRedactor constructs a redactor from a list of compiled patterns
// (use Compile() to compile config-loaded patterns first). nil
// patterns is valid and produces a no-op redactor — convenient for the
// "PII disabled" deployment.
func NewRedactor(patterns []Pattern) *Redactor {
	return &Redactor{
		patterns: patterns,
		maxLen:   MaxPatternLength(patterns),
	}
}

// MaxPatternLength is exposed so the streaming wrapper can size its
// tail buffer to match.
func (r *Redactor) MaxPatternLength() int { return r.maxLen }

// Patterns returns a copy of the configured pattern set so callers can
// iterate without holding the redactor lock. The compiled regexes are
// shared — they are immutable once built.
func (r *Redactor) Patterns() []Pattern {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.patterns)
}

// SetAction overrides the action for a single pattern. Used by the
// /api/pii/patterns/:id admin endpoint and the set_pii_pattern_action
// MCP tool — transient until process restart unless persisted via
// --pii-config.
//
// Publishes a new slice so concurrent Redact callers iterating an
// older snapshot don't race on the per-element Action string (Go
// strings are not atomic two-word values).
func (r *Redactor) SetAction(id string, action Action) error {
	if action != ActionMask && action != ActionBlock && action != ActionRouteLocal {
		return fmt.Errorf("unknown action %q (must be mask, block, or route_local)", action)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.patterns {
		if r.patterns[i].ID == id {
			next := slices.Clone(r.patterns)
			next[i].Action = action
			r.patterns = next
			return nil
		}
	}
	return fmt.Errorf("unknown pattern id %q", id)
}

// Redact is a thin wrapper for callers that don't need per-request
// action overrides. It applies each pattern's compiled-in default
// action.
func (r *Redactor) Redact(text string) Result {
	return r.RedactWithOverrides(text, nil)
}

// RedactWithOverrides scans text and returns the result. The override
// map is keyed by pattern id; when present, the value replaces the
// pattern's compiled-in action for this call only — the redactor's
// stored action is unchanged. Pattern ids missing from the map use
// their stored action.
//
// For every match it records a Span (with HashPrefix, never the value)
// and applies the resolved Action:
//   - block: sets Result.Blocked, leaves text intact (caller decides
//     whether to surface the redacted form).
//   - mask: replaces the span with maskFor(pattern.ID).
//   - route_local: sets Result.LocalOnly, leaves text intact.
//
// Spans are returned in the original input's coordinate system so the
// PIIEvent record can be written without re-running the scan.
func (r *Redactor) RedactWithOverrides(text string, overrides map[string]Action) Result {
	r.mu.RLock()
	patterns := r.patterns
	r.mu.RUnlock()

	if len(patterns) == 0 || text == "" {
		return Result{Redacted: text}
	}

	type rawHit struct {
		patternID string
		action    Action
		start     int
		end       int
	}
	var hits []rawHit

	for _, p := range patterns {
		if p.regex == nil {
			// Pattern declared but Compile() not called. Skip rather
			// than panic; the caller already saw an error from Compile.
			continue
		}
		action := p.Action
		if override, ok := overrides[p.ID]; ok {
			action = override
		}
		idxs := p.regex.FindAllStringIndex(text, -1)
		for _, idx := range idxs {
			candidate := text[idx[0]:idx[1]]
			if VerifyMatch(p.ID, candidate) == "" {
				continue
			}
			hits = append(hits, rawHit{
				patternID: p.ID,
				action:    action,
				start:     idx[0],
				end:       idx[1],
			})
		}
	}

	if len(hits) == 0 {
		return Result{Redacted: text}
	}

	// Sort and deduplicate overlapping hits — when two patterns claim
	// the same span (e.g., a credit-card-shaped value also scans as
	// digits), keep the one with the strongest action. Order: block >
	// route_local > mask. This ensures a deployment that sets the
	// credit-card pattern to "block" wins over a more permissive
	// rule that also covers the same text.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].start != hits[j].start {
			return hits[i].start < hits[j].start
		}
		return actionRank(hits[i].action) > actionRank(hits[j].action)
	})
	merged := hits[:0]
	for _, h := range hits {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if h.start < last.end {
				// Overlap. Extend the existing span and keep the
				// stronger action.
				if actionRank(h.action) > actionRank(last.action) {
					last.action = h.action
					last.patternID = h.patternID
				}
				if h.end > last.end {
					last.end = h.end
				}
				continue
			}
		}
		merged = append(merged, h)
	}

	res := Result{}
	var out strings.Builder
	out.Grow(len(text))
	cursor := 0
	for _, h := range merged {
		matched := text[h.start:h.end]
		span := Span{
			Start:      h.start,
			End:        h.end,
			Pattern:    h.patternID,
			HashPrefix: hashPrefix(matched),
		}
		res.Spans = append(res.Spans, span)

		out.WriteString(text[cursor:h.start])
		switch h.action {
		case ActionBlock:
			res.Blocked = true
			out.WriteString(matched) // leave intact; caller short-circuits
		case ActionRouteLocal:
			res.LocalOnly = true
			out.WriteString(matched)
		default: // ActionMask (and any unknown action defaults to mask)
			out.WriteString(maskFor(h.patternID))
		}
		cursor = h.end
	}
	out.WriteString(text[cursor:])
	res.Redacted = out.String()
	return res
}

// maskFor returns the placeholder that replaces a matched span. The
// shape "[REDACTED:<id>]" is intentionally stable — it surfaces the
// pattern id back to the model, which is sometimes useful (e.g., the
// model can say "I see you redacted an email"). Admins who want a
// less informative replacement can build one in front of this.
func maskFor(patternID string) string {
	return "[REDACTED:" + patternID + "]"
}

// hashPrefix returns the first 8 chars of sha256(value). Two calls
// with the same input produce the same prefix so an admin auditing
// the PIIEvent log can spot a recurring leak ("the same SSN appears
// 200 times this hour") without ever recovering the value.
func hashPrefix(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

func actionRank(a Action) int {
	switch a {
	case ActionBlock:
		return 3
	case ActionRouteLocal:
		return 2
	case ActionMask:
		return 1
	}
	return 0
}
