import { test, expect } from '@playwright/test'

// Regression: clicking the "Create routing model" button on the
// Routing tab loaded the editor with the template's array-shaped
// `router.candidates` value. The code-editor component passes its
// value straight to CodeMirror's EditorState.create({ doc }) which
// expects a string — passing the array crashed inside CM's Text
// class with "(intermediate value).split is not a function" and the
// React router error boundary showed "Unexpected Application Error".
//
// This spec pins the contract that the template loads cleanly and
// renders the candidates editor without throwing. If a future change
// makes the template ship a non-string value through any code-editor
// field without a wrapper, this test fails.

const ROUTER_METADATA = {
  sections: [
    { id: 'general', label: 'General', icon: 'settings', order: 0 },
    { id: 'other', label: 'Other', icon: 'more-horizontal', order: 100 },
  ],
  fields: [
    { path: 'name', yaml_key: 'name', go_type: 'string', ui_type: 'string', section: 'general', label: 'Model Name', component: 'input', order: 0 },
    {
      path: 'router.classifier', yaml_key: 'classifier', go_type: 'string', ui_type: 'string',
      section: 'other', label: 'Router Classifier', component: 'select',
      options: [
        { value: 'feature', label: 'Feature (rules)' },
        { value: 'knn', label: 'KNN (embeddings)' },
        { value: 'llm', label: 'LLM (small instruct)' },
      ],
      order: 230,
    },
    { path: 'router.fallback', yaml_key: 'fallback', go_type: 'string', ui_type: 'string', section: 'other', label: 'Router Fallback', component: 'model-select', autocomplete_provider: 'models:chat', order: 231 },
    { path: 'router.candidates', yaml_key: 'candidates', go_type: '[]RouterCandidate', ui_type: 'object', section: 'other', label: 'Router Candidates', component: 'router-candidates', order: 237 },
  ],
}

const MIDDLEWARE_STATUS = {
  pii: { enabled_globally: false, patterns: [], models: [], recent_event_count: 0 },
  router: { configured: false, models: [], recent_decision_count: 0, available_classifiers: ['feature', 'knn', 'llm'] },
  mitm: { running: false, listen_addr: '', configured_addr: '', host_owners: {}, host_conflicts: {}, models: [], ca_available: false, ca_cert_url: '' },
}

test.describe('Router template — create flow', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/api/auth/status', (route) =>
      route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({ authEnabled: false, staticApiKeyRequired: false, providers: [] }),
      })
    )
    await page.route('**/api/middleware/status', (route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify(MIDDLEWARE_STATUS) })
    )
    await page.route('**/api/router/decisions?**', (route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify({ decisions: [] }) })
    )
    await page.route('**/api/pii/events?**', (route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify({ events: [] }) })
    )
    await page.route('**/api/models/config-metadata*', (route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify(ROUTER_METADATA) })
    )
    await page.route('**/api/models/config-metadata/autocomplete/**', (route) =>
      route.fulfill({ contentType: 'application/json', body: JSON.stringify({ values: [] }) })
    )

    // Surface any uncaught render-time error so the assertion fails
    // with a useful message rather than the test silently passing.
    page.on('pageerror', (err) => {
      throw new Error(`uncaught page error: ${err.message}`)
    })
  })

  test('Routing tab links to the model editor with the router template loaded', async ({ page }) => {
    await page.goto('/app/middleware')
    await page.getByRole('button', { name: /Routing/i }).click()

    // Empty-state button is the primary CTA.
    await page.getByRole('button', { name: /Create routing model/i }).click()

    // Editor loads on a /app/model-editor URL with template=router.
    await expect(page).toHaveURL(/\/app\/model-editor.*template=router/)
  })

  test('Selecting the router template renders the editor without crashing on array candidates', async ({ page }) => {
    // Navigate straight to the create-with-template URL. This is the
    // exact path the "Create routing model" button uses — the same
    // path that crashed with "(intermediate value).split is not a
    // function" when router.candidates was an array.
    await page.goto('/app/model-editor?template=router')

    // The error overlay rendered by react-router includes the literal
    // string "Unexpected Application Error" — its absence is the
    // primary regression assertion.
    await expect(page.getByText(/Unexpected Application Error/i)).toHaveCount(0)

    // Editor surface visible.
    await expect(page.locator('h1', { hasText: 'Model Editor' })).toBeVisible({ timeout: 10_000 })

    // The candidates field (the previously-crashing one) is labelled
    // and visible.
    await expect(page.getByText('Router Candidates').first()).toBeVisible()

    // Structured editor is in use — the "Add candidate" button is the
    // signature of the RouterCandidatesEditor (rather than the raw
    // YAML code-editor). If someone reverts that wiring, this
    // assertion catches it.
    await expect(page.getByRole('button', { name: /Add candidate/i }).first()).toBeVisible()

    // Other router scalar fields populated from the template.
    await expect(page.getByText('Router Classifier').first()).toBeVisible()
  })
})
