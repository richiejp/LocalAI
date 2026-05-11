import SearchableModelSelect from './SearchableModelSelect'
import Toggle from './Toggle'

// RouterCandidatesEditor renders the structured candidate list for a
// router model. Each row is one candidate the classifier can pick.
// The shape mirrors core/config.RouterCandidate:
//
//   {
//     label: string,
//     model: string,
//     description?: string,      // LLM classifier hint
//     rules: {
//       max_prompt_length?: int,
//       min_prompt_length?: int,
//       requires_code?: bool,
//       examples?: string[],     // KNN exemplars
//     }
//   }
//
// We avoid the raw YAML code-editor experience that shipped initially —
// admins shouldn't have to guess the field names, and the model
// dropdown plus inline help here matches the rest of the editor.
//
// Fields not relevant to the chosen classifier (e.g. `description`
// for "feature" / "knn"; `examples` for "feature" / "llm") still
// render — they're cheap to ignore and let admins switch the
// classifier without losing their data. The card header reminds them
// which classifier consumes which field.

export default function RouterCandidatesEditor({ value, onChange }) {
  const items = Array.isArray(value) ? value : []

  const update = (index, mut) => {
    const next = items.map((it, i) => (i === index ? mut({ ...it }) : it))
    onChange(next)
  }
  const remove = (index) => onChange(items.filter((_, i) => i !== index))
  const add = () => onChange([
    ...items,
    { label: '', model: '', rules: {} },
  ])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--spacing-sm)', width: '100%' }}>
      {items.length === 0 && (
        <div style={{ fontSize: '0.75rem', color: 'var(--color-text-muted)', padding: 'var(--spacing-sm) 0' }}>
          No candidates yet. Add at least one — the classifier picks among these for each request.
        </div>
      )}

      {items.map((row, i) => (
        <CandidateRow
          key={i}
          row={row}
          onChange={(mut) => update(i, mut)}
          onRemove={() => remove(i)}
        />
      ))}

      <button
        type="button"
        className="btn btn-secondary btn-sm"
        onClick={add}
        style={{ alignSelf: 'flex-start' }}
      >
        <i className="fas fa-plus" /> Add candidate
      </button>
    </div>
  )
}

function CandidateRow({ row, onChange, onRemove }) {
  const rules = row?.rules || {}
  const examples = Array.isArray(rules.examples) ? rules.examples : []

  const setRule = (key, val) =>
    onChange((r) => {
      r.rules = { ...(r.rules || {}), [key]: val }
      // Clear empty rules so the YAML doesn't carry `max_prompt_length: 0`
      // — the classifier treats 0 as "no upper bound" anyway, but tidy
      // YAML is friendlier to humans reading the file later.
      if (val === '' || val === 0 || val === false) {
        delete r.rules[key]
      }
      return r
    })

  const setExamples = (next) =>
    onChange((r) => {
      r.rules = { ...(r.rules || {}) }
      if (next.length === 0) delete r.rules.examples
      else r.rules.examples = next
      return r
    })

  return (
    <div
      className="card"
      style={{
        padding: 'var(--spacing-sm)',
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--spacing-xs)',
        border: '1px solid var(--color-border)',
      }}
    >
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 2fr auto', gap: 'var(--spacing-sm)', alignItems: 'center' }}>
        <input
          className="input"
          type="text"
          placeholder="label (e.g. code, chat, small)"
          value={row?.label || ''}
          onChange={(e) => onChange((r) => ({ ...r, label: e.target.value }))}
        />
        <SearchableModelSelect
          value={row?.model || ''}
          onChange={(v) => onChange((r) => ({ ...r, model: v }))}
          placeholder="downstream model..."
        />
        <button
          type="button"
          className="btn btn-secondary btn-sm"
          onClick={onRemove}
          title="Remove candidate"
        >
          <i className="fas fa-trash" />
        </button>
      </div>

      <details style={{ marginTop: 4 }}>
        <summary style={{ cursor: 'pointer', fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>
          Rules &amp; classifier hints
        </summary>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--spacing-xs)', marginTop: 6 }}>
          <FieldLabel label="Description (LLM classifier)" hint="One-line natural language hint shown to the LLM classifier alongside this label.">
            <input
              className="input"
              type="text"
              placeholder="e.g. 'use for code-heavy questions'"
              value={row?.description || ''}
              onChange={(e) => onChange((r) => ({ ...r, description: e.target.value || undefined }))}
            />
          </FieldLabel>

          <FieldLabel label="Max prompt length (feature)" hint="Pick this candidate only when the prompt is ≤ N characters. 0 = no upper bound.">
            <input
              className="input"
              type="number"
              min={0}
              value={rules.max_prompt_length || ''}
              onChange={(e) => setRule('max_prompt_length', parseInt(e.target.value, 10) || 0)}
            />
          </FieldLabel>

          <FieldLabel label="Min prompt length (feature)" hint="Pick this candidate only when the prompt is ≥ N characters.">
            <input
              className="input"
              type="number"
              min={0}
              value={rules.min_prompt_length || ''}
              onChange={(e) => setRule('min_prompt_length', parseInt(e.target.value, 10) || 0)}
            />
          </FieldLabel>

          <FieldLabel label="Requires code (feature)" hint="Pick this candidate only when the prompt contains a code fence.">
            <Toggle
              checked={!!rules.requires_code}
              onChange={(v) => setRule('requires_code', v)}
            />
          </FieldLabel>

          <FieldLabel label="Examples (KNN)" hint="Each exemplar is one prompt the KNN classifier embeds and matches against incoming requests. Paste a representative prompt per row — newlines inside a prompt are preserved.">
            <ExamplesEditor value={examples} onChange={setExamples} />
          </FieldLabel>
        </div>
      </details>
    </div>
  )
}

function FieldLabel({ label, hint, children }) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <div style={{ fontSize: '0.75rem', fontWeight: 500 }}>{label}</div>
      {hint && <div style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>{hint}</div>}
      {children}
    </div>
  )
}

// ExamplesEditor renders one resizable textarea per exemplar. The
// previous shape was a single textarea with "one per line"
// semantics, which broke for any exemplar that itself contained a
// newline — a realistic case for multi-line prompts admins want to
// paste in verbatim. One textarea per item lets each exemplar hold
// arbitrary text including line breaks, and the array on the wire
// stays a plain []string the KNN classifier already consumes.
function ExamplesEditor({ value, onChange }) {
  const items = Array.isArray(value) ? value : []

  const update = (index, text) =>
    onChange(items.map((it, i) => (i === index ? text : it)))

  const remove = (index) =>
    onChange(items.filter((_, i) => i !== index))

  const add = () => onChange([...items, ''])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--spacing-xs)' }}>
      {items.length === 0 && (
        <div style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
          No exemplars yet. Add at least one — the KNN classifier needs exemplars to embed.
        </div>
      )}
      {items.map((text, i) => (
        <div key={i} style={{ display: 'flex', gap: 6, alignItems: 'flex-start' }}>
          <textarea
            className="input"
            rows={3}
            placeholder="paste a representative prompt..."
            value={text}
            onChange={(e) => update(i, e.target.value)}
            style={{
              flex: 1,
              fontFamily: 'var(--font-mono)',
              fontSize: '0.75rem',
              resize: 'vertical',
              minHeight: '4em',
            }}
          />
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={() => remove(i)}
            title="Remove exemplar"
            style={{ flexShrink: 0 }}
          >
            <i className="fas fa-trash" />
          </button>
        </div>
      ))}
      <button
        type="button"
        className="btn btn-secondary btn-sm"
        onClick={add}
        style={{ alignSelf: 'flex-start' }}
      >
        <i className="fas fa-plus" /> Add exemplar
      </button>
    </div>
  )
}
