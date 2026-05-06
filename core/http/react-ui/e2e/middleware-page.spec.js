import { test, expect } from '@playwright/test'

// Mocked fixture covering the three things the page renders:
//   - PII pattern catalogue (action badges, action-change buttons)
//   - Per-model resolved PII state (one with default off, one with proxy default on, one with explicit YAML)
//   - Recent events feed (the page must NEVER show the redacted content)
const MOCK_STATUS = {
  pii: {
    enabled_globally: true,
    default_enabled_for_backends: ['proxy-*'],
    patterns: [
      { id: 'email', description: 'Email addresses', action: 'mask', max_match_length: 254 },
      { id: 'ssn', description: 'US Social Security Numbers', action: 'mask', max_match_length: 11 },
      { id: 'api_key_prefix', description: 'API key prefixes', action: 'block', max_match_length: 200 },
    ],
    models: [
      { name: 'qwen-7b', backend: 'llama-cpp', enabled: false, explicit: false, default_for_backend: false, overrides: null },
      { name: 'claude-sonnet', backend: 'proxy-anthropic', enabled: true, explicit: false, default_for_backend: true, overrides: null },
      { name: 'claude-strict', backend: 'proxy-anthropic', enabled: true, explicit: true, default_for_backend: true, overrides: { ssn: 'block' } },
    ],
    recent_event_count: 2,
  },
  router: {
    configured: true,
    models: [
      {
        name: 'smart-router',
        classifier: 'feature',
        fallback: 'qwen-7b',
        candidates: [
          { label: 'small', model: 'qwen-3b', rules: { max_prompt_length: 50, min_prompt_length: 0, requires_code: false } },
          { label: 'code', model: 'qwen-coder', rules: { max_prompt_length: 0, min_prompt_length: 0, requires_code: true } },
          { label: 'large', model: 'qwen-32b', rules: { max_prompt_length: 0, min_prompt_length: 0, requires_code: false } },
        ],
      },
    ],
    recent_decision_count: 1,
    available_classifiers: ['feature'],
  },
}

const MOCK_DECISIONS = {
  decisions: [
    {
      id: 'rd_a1', correlation_id: 'corr-1', user_id: 'local',
      router_model: 'smart-router', requested_model: 'smart-router', served_model: 'qwen-3b',
      classifier: 'feature', label: 'small', score: 1.0, latency_ms: 2, cached: false,
      created_at: '2026-05-06T11:00:00Z',
    },
  ],
}

const MOCK_EVENTS = {
  events: [
    {
      id: 'pii_aaa', correlation_id: 'corr-1', user_id: 'local',
      direction: 'in', pattern_id: 'email', byte_offset: 12, length: 17,
      hash_prefix: 'ff8d9819', action: 'mask',
      created_at: '2026-05-06T10:00:00Z',
    },
  ],
}

test.describe('Middleware page — admin in no-auth mode', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/api/auth/status', (route) =>
      route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({ authEnabled: false, staticApiKeyRequired: false, providers: [] }),
      })
    )
    await page.route('**/api/middleware/status', (route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify(MOCK_STATUS) })
    )
    await page.route('**/api/pii/events?**', (route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify(MOCK_EVENTS) })
    )
    await page.route('**/api/router/decisions?**', (route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify(MOCK_DECISIONS) })
    )
  })

  test('Filtering tab renders pattern catalogue and per-model state', async ({ page }) => {
    await page.goto('/app/middleware')

    // Pattern table — at least one pattern id visible.
    await expect(page.getByText('email').first()).toBeVisible()
    await expect(page.getByText('api_key_prefix').first()).toBeVisible()

    // Per-model state — each model's name is visible.
    await expect(page.getByText('qwen-7b').first()).toBeVisible()
    await expect(page.getByText('claude-strict').first()).toBeVisible()

    // Default-policy banner mentions proxy-*.
    await expect(page.getByText(/proxy-\*/).first()).toBeVisible()
  })

  test('Routing tab renders configured routers and recent decisions', async ({ page }) => {
    await page.goto('/app/middleware')
    await page.getByRole('button', { name: /Routing/i }).click()
    // Active router model name visible.
    await expect(page.getByText('smart-router').first()).toBeVisible()
    // Candidate model name visible (one of three).
    await expect(page.getByText('qwen-coder').first()).toBeVisible()
    // Decision row visible — label and served model.
    await expect(page.getByText('small').first()).toBeVisible()
    await expect(page.getByText('qwen-3b').first()).toBeVisible()
  })

  test('Events tab renders rows but never the redacted content', async ({ page }) => {
    await page.goto('/app/middleware')
    await page.getByRole('button', { name: /Events/i }).click()
    // Hash prefix is visible — that's how admins audit recurring leaks.
    await expect(page.getByText('ff8d9819')).toBeVisible()
    // The page only ever shows fields the EventStore stores. The matched
    // value (e.g. "alice@example.com") would never appear because it's
    // not in the payload — explicit asserting absence here is the
    // contract the design relies on.
    await expect(page.getByText(/@example\.com/)).toHaveCount(0)
  })

  test('PUT /api/pii/patterns/:id fires when an action button is clicked', async ({ page }) => {
    let putHit = null
    await page.route('**/api/pii/patterns/email', (route) => {
      if (route.request().method() === 'PUT') {
        putHit = JSON.parse(route.request().postData() || '{}')
        route.fulfill({ contentType: 'application/json', body: JSON.stringify({ id: 'email', action: putHit.action, persisted: false }) })
      } else {
        route.continue()
      }
    })

    await page.goto('/app/middleware')
    // Click the email row's "block" button (currently mask, so block is
    // enabled). Use a precise locator that matches the inner button.
    const emailRow = page.locator('tr').filter({ hasText: 'email' }).first()
    await emailRow.getByRole('button', { name: 'block' }).click()

    await expect.poll(() => putHit).toEqual({ action: 'block' })
  })
})

test.describe('Middleware page — non-admin under auth-on', () => {
  test('redirects to /app when the user is not admin', async ({ page }) => {
    await page.route('**/api/auth/status', (route) =>
      route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({
          authEnabled: true,
          staticApiKeyRequired: false,
          providers: ['local'],
          user: { id: 'bob', name: 'Bob', role: 'user', provider: 'local' },
        }),
      })
    )

    await page.goto('/app/middleware')
    // RequireAdmin redirects non-admin viewers; the URL must not stay on /middleware.
    await page.waitForURL(/\/app(?!\/middleware)/, { timeout: 5000 })
    expect(page.url()).not.toMatch(/\/middleware/)
  })
})
