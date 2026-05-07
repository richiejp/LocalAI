import { useState, useEffect, useCallback } from 'react'
import { useOutletContext } from 'react-router-dom'
import { apiUrl } from '../utils/basePath'
import { settingsApi } from '../utils/api'
import LoadingSpinner from '../components/LoadingSpinner'

// Middleware admin page. Three tabs:
//   - Filtering: PII pattern catalogue + per-model resolved state +
//     pattern-action editor (PUT /api/pii/patterns/:id, transient).
//   - Routing: placeholder until subsystem 2 lands. Renders the note
//     from /api/router/status so admins see "not yet implemented" rather
//     than an empty page.
//   - Events: recent PIIEvent rows from /api/pii/events. The page
//     intentionally NEVER displays the redacted content (the redactor
//     never stores it); only pattern_id, byte_offset, length, and an
//     8-char sha256 prefix admins can use to dedupe recurring leaks.
//
// Wiring is admin-only: RequireAdmin in router.jsx already redirects
// non-admin viewers; in single-user no-auth mode the local user has
// admin role so the page works without --auth.

const TABS = [
  { id: 'filtering', label: 'Filtering', icon: 'fa-shield-halved' },
  { id: 'routing', label: 'Routing', icon: 'fa-route' },
  { id: 'proxy', label: 'MITM Proxy', icon: 'fa-shield' },
  { id: 'events', label: 'Events', icon: 'fa-list-ul' },
]

const ACTIONS = ['mask', 'block', 'route_local']

function actionBadge(action) {
  const colors = {
    mask: 'var(--color-primary)',
    block: 'var(--color-error)',
    route_local: 'var(--color-warning)',
  }
  return (
    <span style={{
      display: 'inline-block',
      padding: '2px 8px',
      fontSize: '0.6875rem',
      fontWeight: 600,
      borderRadius: 'var(--radius-sm)',
      background: colors[action] || 'var(--color-bg-tertiary)',
      color: 'white',
      fontFamily: 'var(--font-mono)',
      textTransform: 'uppercase',
    }}>
      {action}
    </span>
  )
}

function enabledBadge(enabled) {
  return (
    <span style={{
      display: 'inline-block',
      padding: '2px 8px',
      fontSize: '0.6875rem',
      fontWeight: 600,
      borderRadius: 'var(--radius-sm)',
      background: enabled ? 'var(--color-success, #22c55e)' : 'var(--color-bg-tertiary)',
      color: enabled ? 'white' : 'var(--color-text-muted)',
      fontFamily: 'var(--font-mono)',
      textTransform: 'uppercase',
    }}>
      {enabled ? 'on' : 'off'}
    </span>
  )
}

export default function Middleware() {
  const { addToast } = useOutletContext()
  const [status, setStatus] = useState(null)
  const [events, setEvents] = useState([])
  const [decisions, setDecisions] = useState([])
  const [loading, setLoading] = useState(true)
  const [activeTab, setActiveTab] = useState('filtering')
  const [pendingPattern, setPendingPattern] = useState(null) // id while a PUT is in flight

  const fetchAll = useCallback(async () => {
    setLoading(true)
    try {
      const [statusRes, eventsRes, decisionsRes] = await Promise.all([
        fetch(apiUrl('/api/middleware/status')),
        fetch(apiUrl('/api/pii/events?limit=100')),
        fetch(apiUrl('/api/router/decisions?limit=100')),
      ])
      if (!statusRes.ok) throw new Error(`status: HTTP ${statusRes.status}`)
      const statusData = await statusRes.json()
      setStatus(statusData)
      if (eventsRes.ok) {
        const data = await eventsRes.json()
        setEvents(data.events || [])
      }
      if (decisionsRes.ok) {
        const data = await decisionsRes.json()
        setDecisions(data.decisions || [])
      }
    } catch (err) {
      addToast(`Failed to load middleware status: ${err.message}`, 'error')
    } finally {
      setLoading(false)
    }
  }, [addToast])

  useEffect(() => { fetchAll() }, [fetchAll])

  const setPatternAction = async (patternID, action) => {
    setPendingPattern(patternID)
    try {
      const res = await fetch(apiUrl(`/api/pii/patterns/${encodeURIComponent(patternID)}`), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action }),
      })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body.error || `HTTP ${res.status}`)
      }
      addToast(`Pattern ${patternID}: action set to ${action} (transient until restart)`, 'success')
      await fetchAll()
    } catch (err) {
      addToast(`Failed to set action: ${err.message}`, 'error')
    } finally {
      setPendingPattern(null)
    }
  }

  return (
    <div className="page page--wide">
      <div className="page-header" style={{ marginBottom: 'var(--spacing-sm)' }}>
        <h1 className="page-title">Middleware</h1>
        <p className="page-subtitle">
          Inspect and configure routing-module middleware: PII filtering and intelligent routing.
        </p>
      </div>

      {/* Tab bar */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--spacing-xs)', marginBottom: 'var(--spacing-md)', flexWrap: 'wrap' }}>
        {TABS.map(tab => (
          <button
            key={tab.id}
            className={`btn btn-sm ${activeTab === tab.id ? 'btn-primary' : 'btn-secondary'}`}
            onClick={() => setActiveTab(tab.id)}
          >
            <i className={`fas ${tab.icon}`} style={{ marginRight: 4 }} />
            {tab.label}
          </button>
        ))}
        <div style={{ flex: 1 }} />
        <button className="btn btn-secondary btn-sm" onClick={fetchAll} disabled={loading}>
          <i className={`fas fa-rotate${loading ? ' fa-spin' : ''}`} /> Refresh
        </button>
      </div>

      {loading && !status ? (
        <div style={{ display: 'flex', justifyContent: 'center', padding: 'var(--spacing-xl)' }}>
          <LoadingSpinner size="lg" />
        </div>
      ) : activeTab === 'filtering' ? (
        <FilteringTab
          status={status}
          pendingPattern={pendingPattern}
          onSetAction={setPatternAction}
        />
      ) : activeTab === 'routing' ? (
        <RoutingTab status={status} decisions={decisions} />
      ) : activeTab === 'proxy' ? (
        <ProxyTab status={status} addToast={addToast} onChanged={fetchAll} />
      ) : (
        <EventsTab events={events} />
      )}
    </div>
  )
}

function FilteringTab({ status, pendingPattern, onSetAction }) {
  if (!status?.pii) return null
  const pii = status.pii

  if (!pii.enabled_globally) {
    return (
      <div className="empty-state">
        <div className="empty-state-icon"><i className="fas fa-shield-slash" /></div>
        <h2 className="empty-state-title">PII filtering disabled</h2>
        <p className="empty-state-text">
          The PII filter is disabled by <code>{pii.reason || '--disable-pii'}</code>.
          Restart without that flag to enable it.
        </p>
      </div>
    )
  }

  return (
    <>
      {/* Default rule banner */}
      <div className="card" style={{ padding: 'var(--spacing-md)', marginBottom: 'var(--spacing-md)' }}>
        <div style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--spacing-sm)' }}>
          <i className="fas fa-info-circle" style={{ color: 'var(--color-text-muted)', marginTop: 2 }} />
          <div>
            <div style={{ fontWeight: 600, marginBottom: 4 }}>Default policy</div>
            <div style={{ fontSize: '0.8125rem', color: 'var(--color-text-secondary)' }}>
              PII redaction is per-model and OFF by default. Backends matching <code>{(pii.default_enabled_for_backends || []).join(', ')}</code> default to ON (cloud passthroughs). Override per model with <code>pii: {'{'} enabled: true {'}'}</code> in the model YAML.
            </div>
          </div>
        </div>
      </div>

      {/* Patterns table */}
      <div className="card" style={{ padding: 'var(--spacing-md)', marginBottom: 'var(--spacing-md)' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--spacing-sm)' }}>
          <span style={{ fontSize: '0.875rem', fontWeight: 600 }}>Active patterns</span>
          <span style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
            Action changes are transient — restored to YAML defaults on restart.
          </span>
        </div>
        <div className="table-container">
          <table className="table">
            <thead>
              <tr>
                <th style={{ width: 140 }}>Pattern</th>
                <th>Description</th>
                <th style={{ width: 110 }}>Action</th>
                <th style={{ width: 250 }}>Change</th>
              </tr>
            </thead>
            <tbody>
              {pii.patterns.map(p => (
                <tr key={p.id}>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.8125rem', fontWeight: 600 }}>{p.id}</td>
                  <td style={{ fontSize: '0.8125rem', color: 'var(--color-text-secondary)' }}>{p.description}</td>
                  <td>{actionBadge(p.action)}</td>
                  <td>
                    <div style={{ display: 'flex', gap: 4 }}>
                      {ACTIONS.map(a => (
                        <button
                          key={a}
                          className={`btn btn-sm ${p.action === a ? 'btn-primary' : 'btn-secondary'}`}
                          onClick={() => onSetAction(p.id, a)}
                          disabled={pendingPattern === p.id || p.action === a}
                          style={{ fontSize: '0.6875rem', padding: '2px 8px' }}
                        >
                          {a}
                        </button>
                      ))}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {/* Per-model resolved state */}
      <div className="card" style={{ padding: 'var(--spacing-md)' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--spacing-sm)' }}>
          <span style={{ fontSize: '0.875rem', fontWeight: 600 }}>Per-model state</span>
          <span style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
            Edit the model YAML to change these.
          </span>
        </div>
        <div className="table-container">
          <table className="table">
            <thead>
              <tr>
                <th>Model</th>
                <th style={{ width: 120 }}>Backend</th>
                <th style={{ width: 80 }}>PII</th>
                <th style={{ width: 110 }}>Source</th>
                <th>Pattern overrides</th>
              </tr>
            </thead>
            <tbody>
              {(pii.models || []).map(m => (
                <tr key={m.name}>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.8125rem' }}>{m.name}</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>{m.backend || '—'}</td>
                  <td>{enabledBadge(m.enabled)}</td>
                  <td style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
                    {m.explicit ? 'YAML' : (m.default_for_backend ? 'backend default' : 'default off')}
                  </td>
                  <td style={{ fontSize: '0.75rem', fontFamily: 'var(--font-mono)' }}>
                    {m.overrides && Object.keys(m.overrides).length > 0
                      ? Object.entries(m.overrides).map(([k, v]) => `${k}=${v}`).join(', ')
                      : <span style={{ color: 'var(--color-text-muted)' }}>—</span>}
                  </td>
                </tr>
              ))}
              {(!pii.models || pii.models.length === 0) && (
                <tr>
                  <td colSpan={5} style={{ textAlign: 'center', color: 'var(--color-text-muted)', padding: 'var(--spacing-md)' }}>
                    No models loaded.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>
    </>
  )
}

function RoutingTab({ status, decisions }) {
  const router = status?.router || { configured: false }

  if (!router.configured || !router.models || router.models.length === 0) {
    return (
      <div className="empty-state">
        <div className="empty-state-icon"><i className="fas fa-route" /></div>
        <h2 className="empty-state-title">No routers configured</h2>
        <p className="empty-state-text">
          {router.note || 'Add a `router:` block to a model YAML to enable intelligent routing. The classifier picks one of the listed candidates per request and the standard model-resolution path runs against the chosen target.'}
        </p>
      </div>
    )
  }

  return (
    <>
      {/* Configured router models */}
      <div className="card" style={{ padding: 'var(--spacing-md)', marginBottom: 'var(--spacing-md)' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--spacing-sm)' }}>
          <span style={{ fontSize: '0.875rem', fontWeight: 600 }}>Active routers</span>
          <span style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
            Edit the router model YAML to change candidates or rules.
          </span>
        </div>
        <div className="table-container">
          <table className="table">
            <thead>
              <tr>
                <th style={{ width: 160 }}>Model</th>
                <th style={{ width: 110 }}>Classifier</th>
                <th>Candidates</th>
                <th style={{ width: 140 }}>Fallback</th>
              </tr>
            </thead>
            <tbody>
              {router.models.map(m => (
                <tr key={m.name}>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.8125rem', fontWeight: 600 }}>{m.name}</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem' }}>{m.classifier}</td>
                  <td style={{ fontSize: '0.75rem' }}>
                    {(m.candidates || []).map((c, i) => (
                      <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: 'var(--font-mono)' }}>
                        <span style={{ minWidth: 60, color: 'var(--color-primary)' }}>{c.label}</span>
                        <span style={{ color: 'var(--color-text-muted)' }}>→</span>
                        <span>{c.model}</span>
                        {(c.rules?.max_prompt_length || c.rules?.min_prompt_length || c.rules?.requires_code) && (
                          <span style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)', marginLeft: 6 }}>
                            ({[
                              c.rules.requires_code && 'code',
                              c.rules.max_prompt_length > 0 && `≤${c.rules.max_prompt_length}c`,
                              c.rules.min_prompt_length > 0 && `≥${c.rules.min_prompt_length}c`,
                            ].filter(Boolean).join(', ')})
                          </span>
                        )}
                      </div>
                    ))}
                  </td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>
                    {m.fallback || '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {/* Recent decisions */}
      <div className="card" style={{ padding: 'var(--spacing-md)' }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--spacing-sm)' }}>
          <span style={{ fontSize: '0.875rem', fontWeight: 600 }}>Recent decisions</span>
          <span style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
            Newest first, capped at 100.
          </span>
        </div>
        {(!decisions || decisions.length === 0) ? (
          <div style={{ padding: 'var(--spacing-md)', textAlign: 'center', color: 'var(--color-text-muted)', fontSize: '0.8125rem' }}>
            No routing decisions yet. Send a request to a router model to populate this log.
          </div>
        ) : (
          <div className="table-container">
            <table className="table">
              <thead>
                <tr>
                  <th style={{ width: 170 }}>Time</th>
                  <th style={{ width: 130 }}>Router</th>
                  <th style={{ width: 80 }}>Label</th>
                  <th style={{ width: 130 }}>Served</th>
                  <th style={{ width: 90 }}>Latency</th>
                  <th>Correlation</th>
                </tr>
              </thead>
              <tbody>
                {decisions.map(d => (
                  <tr key={d.id}>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>{d.created_at}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem' }}>{d.router_model}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem', fontWeight: 600 }}>{d.label}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem' }}>{d.served_model}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>{d.latency_ms}ms</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
                      {d.correlation_id || '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}

function ProxyTab({ status, addToast, onChanged }) {
  const mitm = status?.mitm
  const serverListen = mitm?.configured_addr || ''
  const serverHosts = (mitm?.intercept_hosts || []).join(', ')

  const [listen, setListen] = useState(serverListen)
  const [hosts, setHosts] = useState(serverHosts)
  const [saving, setSaving] = useState(false)

  const dirty = listen !== serverListen || hosts !== serverHosts

  // Refresh local state from the server only when (a) the server-side
  // values actually changed (string compare, not array reference) and
  // (b) the user has no pending edits to clobber. Without the dirty
  // gate, a Refresh / post-save refetch wipes mid-typed input.
  useEffect(() => {
    if (dirty) return
    setListen(serverListen)
    setHosts(serverHosts)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [serverListen, serverHosts])

  const save = async () => {
    setSaving(true)
    try {
      const parsedHosts = hosts.split(/[,\s]+/).map(h => h.trim()).filter(Boolean)
      const body = await settingsApi.save({
        mitm_listen: listen,
        mitm_intercept_hosts: parsedHosts,
      })
      if (body && body.success === false) {
        throw new Error(body.error || 'unknown error')
      }
      addToast('MITM proxy settings updated', 'success')
      onChanged?.()
    } catch (err) {
      addToast(`Failed to save: ${err.message}`, 'error')
    } finally {
      setSaving(false)
    }
  }

  if (!mitm) {
    return (
      <div className="empty-state">
        <div className="empty-state-icon"><i className="fas fa-shield" /></div>
        <h2 className="empty-state-title">MITM proxy status unavailable</h2>
        <p className="empty-state-text">The status endpoint did not return a mitm section.</p>
      </div>
    )
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--spacing-md)' }}>
      <div className="card" style={{ padding: 'var(--spacing-lg)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--spacing-md)', marginBottom: 'var(--spacing-md)' }}>
          <h2 style={{ fontSize: '1rem', fontWeight: 600, margin: 0 }}>State</h2>
          {enabledBadge(mitm.running)}
          {mitm.running && (
            <span style={{ fontFamily: 'var(--font-mono)', fontSize: '0.8125rem', color: 'var(--color-text-secondary)' }}>
              listening on {mitm.listen_addr}
            </span>
          )}
        </div>
        <p style={{ fontSize: '0.8125rem', color: 'var(--color-text-secondary)', marginBottom: 'var(--spacing-sm)' }}>
          The MITM proxy terminates TLS for allowlisted hosts so PII redaction
          can run on traffic from clients that authenticate via OAuth /
          subscription (Claude Code, Codex CLI). Non-allowlisted hosts get a
          plain CONNECT tunnel — no inspection, no CA-trust required.
        </p>
        {mitm.ca_available ? (
          <a
            className="btn btn-secondary btn-sm"
            href={apiUrl(mitm.ca_cert_url)}
            download="localai-mitm-ca.crt"
          >
            <i className="fas fa-download" /> Download CA cert
          </a>
        ) : (
          <span style={{ fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>
            CA not generated yet — start the listener to generate it.
          </span>
        )}
      </div>

      <div className="card" style={{ padding: 'var(--spacing-lg)' }}>
        <h2 style={{ fontSize: '1rem', fontWeight: 600, marginTop: 0, marginBottom: 'var(--spacing-md)' }}>Configuration</h2>

        <label style={{ display: 'block', marginBottom: 'var(--spacing-md)' }}>
          <div style={{ fontSize: '0.875rem', fontWeight: 500, marginBottom: 'var(--spacing-xs)' }}>Listen address</div>
          <input
            type="text"
            value={listen}
            onChange={e => setListen(e.target.value)}
            placeholder=":8443  (leave empty to disable)"
            style={{ width: '100%', padding: '8px 12px', fontFamily: 'var(--font-mono)', fontSize: '0.875rem', background: 'var(--color-bg-tertiary)', border: '1px solid var(--color-border-default)', borderRadius: 'var(--radius-sm)', color: 'var(--color-text-primary)' }}
          />
          <div style={{ marginTop: 'var(--spacing-xs)', fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>
            Bind address for the proxy listener. Empty disables it. Bind to <code>127.0.0.1:port</code> unless the listener is reachable only from clients you control — there is no auth on the CONNECT port.
          </div>
        </label>

        <label style={{ display: 'block', marginBottom: 'var(--spacing-md)' }}>
          <div style={{ fontSize: '0.875rem', fontWeight: 500, marginBottom: 'var(--spacing-xs)' }}>Intercept hosts</div>
          <textarea
            rows={3}
            value={hosts}
            onChange={e => setHosts(e.target.value)}
            placeholder="api.anthropic.com, api.openai.com"
            style={{ width: '100%', padding: '8px 12px', fontFamily: 'var(--font-mono)', fontSize: '0.875rem', background: 'var(--color-bg-tertiary)', border: '1px solid var(--color-border-default)', borderRadius: 'var(--radius-sm)', color: 'var(--color-text-primary)', resize: 'vertical' }}
          />
          <div style={{ marginTop: 'var(--spacing-xs)', fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>
            Comma- or whitespace-separated hostnames the proxy terminates TLS for. Hosts not listed get a plain CONNECT tunnel.
          </div>
        </label>

        <div style={{ display: 'flex', gap: 'var(--spacing-sm)' }}>
          <button
            className="btn btn-primary btn-sm"
            onClick={save}
            disabled={!dirty || saving}
          >
            <i className={`fas ${saving ? 'fa-spinner fa-spin' : 'fa-save'}`} /> {saving ? 'Saving…' : 'Apply'}
          </button>
          {dirty && (
            <button
              className="btn btn-ghost btn-sm"
              onClick={() => {
                setListen(mitm.configured_addr || '')
                setHosts((mitm.intercept_hosts || []).join(', '))
              }}
              disabled={saving}
            >
              Discard changes
            </button>
          )}
        </div>
      </div>

      <div className="card" style={{ padding: 'var(--spacing-md)', background: 'var(--color-bg-secondary)' }}>
        <h2 style={{ fontSize: '0.875rem', fontWeight: 600, marginTop: 0, marginBottom: 'var(--spacing-sm)' }}>Client setup</h2>
        <ol style={{ margin: 0, paddingLeft: 20, fontSize: '0.8125rem', color: 'var(--color-text-secondary)', lineHeight: 1.7 }}>
          <li>Download the CA cert (button above).</li>
          <li>Trust it on the client. For Node-based CLIs (Claude Code, Codex): <code style={{ fontFamily: 'var(--font-mono)' }}>export NODE_EXTRA_CA_CERTS=$(pwd)/localai-mitm-ca.crt</code></li>
          <li>Point the client at the proxy: <code style={{ fontFamily: 'var(--font-mono)' }}>export HTTPS_PROXY=http://&lt;host&gt;:&lt;port&gt;</code></li>
        </ol>
      </div>
    </div>
  )
}

function EventsTab({ events }) {
  if (!events || events.length === 0) {
    return (
      <div className="empty-state">
        <div className="empty-state-icon"><i className="fas fa-list-ul" /></div>
        <h2 className="empty-state-title">No PII events</h2>
        <p className="empty-state-text">
          Events appear here when the redactor matches a pattern. The matched value is never stored —
          only an 8-char sha256 prefix admins can use to dedupe recurring leaks.
        </p>
      </div>
    )
  }
  return (
    <div className="card" style={{ padding: 'var(--spacing-md)' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 'var(--spacing-sm)' }}>
        <span style={{ fontSize: '0.875rem', fontWeight: 600 }}>Recent events</span>
        <span style={{ fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
          Newest first, capped at 100.
        </span>
      </div>
      <div className="table-container">
        <table className="table">
          <thead>
            <tr>
              <th style={{ width: 170 }}>Time</th>
              <th style={{ width: 110 }}>Pattern</th>
              <th style={{ width: 110 }}>Action</th>
              <th style={{ width: 80 }}>Length</th>
              <th style={{ width: 110 }}>Hash prefix</th>
              <th>Correlation</th>
            </tr>
          </thead>
          <tbody>
            {events.map(e => (
              <tr key={e.id}>
                <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem', color: 'var(--color-text-muted)' }}>
                  {e.created_at}
                </td>
                <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.8125rem', fontWeight: 600 }}>{e.pattern_id}</td>
                <td>{actionBadge(e.action)}</td>
                <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem' }}>{e.length}</td>
                <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.75rem' }}>{e.hash_prefix}</td>
                <td style={{ fontFamily: 'var(--font-mono)', fontSize: '0.6875rem', color: 'var(--color-text-muted)' }}>
                  {e.correlation_id || '—'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
