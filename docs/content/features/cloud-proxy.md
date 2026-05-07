+++
title = "Cloud passthrough proxy"
weight = 28
toc = true
description = "Forward requests to OpenAI, Anthropic, or any compatible provider"
tags = ["Proxy", "Cloud", "Routing", "Advanced"]
categories = ["Features"]
+++

LocalAI can forward chat-completion and Anthropic Messages requests to an
external provider instead of running them through the local gRPC backend
pipeline. Configure a model whose `backend` starts with `proxy-` and whose
`proxy.upstream_url` is set, and LocalAI bypasses templating, MCP injection,
and the local model loader entirely — the upstream sees the body the client
sent (with only the top-level `model` field optionally rewritten).

The streaming PII filter still runs over the upstream's SSE stream, so cloud
egress remains subject to the same redaction rules a local model would apply.

## When to use this

- Mix local and cloud models in the same LocalAI instance — clients hit one
  endpoint, LocalAI dispatches per model.
- Apply LocalAI's auth, usage tracking, and PII redaction to cloud traffic
  before the body leaves the network.
- Use the intelligent router to send small or simple prompts to a local model
  and complex ones to Claude or GPT-4o.

## How it works

1. Request hits LocalAI on `/v1/chat/completions` (OpenAI-shaped) or
   `/v1/messages` (Anthropic-shaped).
2. The standard auth and routing middleware runs.
3. Per-model PII redaction runs request-side as it would for any model.
4. The handler checks `IsCloudProxy()`. If true, it skips the gRPC backend
   and hands the request body to the cloudproxy package.
5. The proxy POSTs the body to `proxy.upstream_url` with provider-aware
   authentication, then streams the SSE response back to the client.
6. The streaming PII filter rewrites per-token text in flight; the upstream's
   event names and metadata pass through unchanged.

The proxy is **wire-format-faithful** — it does not translate request shapes
between providers. A client posting an OpenAI-shaped body to a `proxy-anthropic`
model will get a confused upstream. Use the proxy that matches your client's
wire format.

## Configuration

Two backends are supported in the MVP:

| Backend | Wire shape | Endpoint | Auth |
|---|---|---|---|
| `proxy-openai` | OpenAI chat completions | `/v1/chat/completions` | `Authorization: Bearer $KEY` |
| `proxy-anthropic` | Anthropic Messages | `/v1/messages` | `x-api-key: $KEY` plus `anthropic-version` header |

API keys are read from environment variables named in the model's YAML.
The key never appears in the config file or the admin UI.

### OpenAI passthrough

```yaml
name: gpt-4o-proxy
backend: proxy-openai

# When set, replaces the client's "model" field before forwarding.
# Useful when the LocalAI alias differs from the upstream's canonical name.
proxy:
  upstream_url: https://api.openai.com/v1/chat/completions
  api_key_env: OPENAI_API_KEY
  upstream_model: gpt-4o
  request_timeout_seconds: 120

# PII filtering defaults to ON for proxy-* backends. Override by setting
# pii.enabled: false explicitly. Per-pattern action overrides go in
# pii.patterns; see the Middleware admin page or features/middleware.md.
pii:
  enabled: true
```

Then start LocalAI with the API key in the environment:

```bash
export OPENAI_API_KEY=sk-...
local-ai run
```

Clients hit `http://localhost:8080/v1/chat/completions` with `"model": "gpt-4o-proxy"`
and the request lands on OpenAI's API.

### Anthropic passthrough

```yaml
name: claude-sonnet-proxy
backend: proxy-anthropic

proxy:
  upstream_url: https://api.anthropic.com/v1/messages
  api_key_env: ANTHROPIC_API_KEY
  upstream_model: claude-3-5-sonnet-20241022
  request_timeout_seconds: 300

pii:
  enabled: true
  # Block — not just mask — leaked credentials before they reach the upstream.
  patterns:
    - id: api_key_prefix
      action: block
```

Anthropic clients hit `http://localhost:8080/v1/messages` with
`"model": "claude-sonnet-proxy"`.

### Other OpenAI-compatible providers

Most third-party providers (Together, Groq, DeepInfra, OpenRouter, …) speak
the OpenAI chat-completions wire format. Use `backend: proxy-openai` with the
provider's URL and API key:

```yaml
name: llama-3-70b-via-together
backend: proxy-openai

proxy:
  upstream_url: https://api.together.xyz/v1/chat/completions
  api_key_env: TOGETHER_API_KEY
  upstream_model: meta-llama/Llama-3-70b-chat-hf
```

## Combining with the intelligent router

A router model can spread traffic across local and cloud candidates:

```yaml
name: smart-router
backend: virtual
router:
  classifier: feature
  fallback: qwen-3-7b-local
  candidates:
    - label: simple
      model: qwen-3-7b-local
      rules:
        max_prompt_length: 2000
    - label: complex
      model: claude-sonnet-proxy
      rules:
        min_prompt_length: 2000
    - label: code
      model: gpt-4o-proxy
      rules:
        requires_code: true
```

The router rewrites `input.Model` to the chosen candidate; per-model PII,
ACLs, and the cloud-proxy fork all run against the resolved target.

## Limitations in the MVP

- **No request-shape translation.** A `proxy-anthropic` model only accepts
  Anthropic-shaped requests; a `proxy-openai` model only accepts OpenAI-shaped
  ones.
- **No output-side PII for non-streaming responses.** Streaming responses are
  filtered in flight; buffered responses pass through verbatim. Request-side
  PII covers both.
- **No retry or backoff.** Transient upstream failures bubble up to the client
  as `502 Bad Gateway`.
- **No request shape validation.** If the upstream rejects the body, its
  error envelope is forwarded to the client unchanged.

## Operational notes

- The fork happens before all local pipeline work, so cloud-proxy models do
  not load gRPC backends. They consume no GPU memory and don't appear in the
  VRAM admin view.
- Usage stats and the trace log capture cloud-proxy requests like any other
  request. Token counts come from the upstream's `usage` field when present.
- Set `request_timeout_seconds` defensively — a hung upstream otherwise ties
  up an HTTP handler until the client disconnects.
