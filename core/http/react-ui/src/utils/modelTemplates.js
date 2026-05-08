// Model templates for the "Add Model" create flow.
// Each template pre-populates the Model Editor with relevant fields.

const MODEL_TEMPLATES = [
  {
    id: 'other',
    label: 'Other',
    icon: 'fa-file-alt',
    description: 'Blank configuration — add any fields you need',
    fields: {
      'name': '',
    },
  },
  {
    id: 'pipeline',
    label: 'Voice Pipeline',
    icon: 'fa-diagram-project',
    description: 'Real-time voice pipeline combining VAD, transcription, LLM, and TTS models',
    fields: {
      'name': '',
      'pipeline.vad': '',
      'pipeline.transcription': '',
      'pipeline.llm': '',
      'pipeline.tts': '',
      'tts.voice': '',
    },
  },
  {
    id: 'llm',
    label: 'LLM',
    icon: 'fa-brain',
    description: 'Language model for chat and text completion',
    fields: {
      'name': '',
      'backend': '',
      'parameters.model': '',
      'context_size': 0,
    },
  },
  {
    id: 'tts',
    label: 'TTS',
    icon: 'fa-volume-up',
    description: 'Text-to-speech model for voice synthesis',
    fields: {
      'name': '',
      'backend': '',
      'parameters.model': '',
      'tts.voice': '',
    },
  },
  {
    id: 'image',
    label: 'Image Generation',
    icon: 'fa-image',
    description: 'Image generation model using diffusers or other backends',
    fields: {
      'name': '',
      'backend': 'diffusers',
      'parameters.model': '',
      'diffusers.pipeline_type': '',
      'diffusers.cuda': false,
    },
  },
  {
    id: 'embedding',
    label: 'Embedding',
    icon: 'fa-vector-square',
    description: 'Embedding model for text vectorization',
    fields: {
      'name': '',
      'backend': '',
      'parameters.model': '',
      'embeddings': true,
    },
  },
  {
    id: 'proxy-openai',
    label: 'OpenAI Proxy',
    icon: 'fa-cloud',
    description: 'Forward chat completions to OpenAI or any OpenAI-compatible provider; PII redaction runs in flight',
    fields: {
      'name': '',
      'backend': 'proxy-openai',
      'proxy.upstream_url': 'https://api.openai.com/v1/chat/completions',
      'proxy.api_key_env': 'OPENAI_API_KEY',
      'proxy.upstream_model': '',
      'proxy.request_timeout_seconds': 120,
      'pii.enabled': true,
    },
  },
  {
    id: 'proxy-anthropic',
    label: 'Anthropic Proxy',
    icon: 'fa-cloud',
    description: 'Forward Messages API requests to Anthropic; PII redaction runs in flight',
    fields: {
      'name': '',
      'backend': 'proxy-anthropic',
      'proxy.upstream_url': 'https://api.anthropic.com/v1/messages',
      'proxy.api_key_env': 'ANTHROPIC_API_KEY',
      'proxy.upstream_model': '',
      'proxy.request_timeout_seconds': 300,
      'pii.enabled': true,
    },
  },
  {
    id: 'mitm',
    label: 'MITM Intercept',
    icon: 'fa-shield-halved',
    description: 'Bind a hostname to this config for the cloudproxy MITM listener. PII filtering and pattern overrides flow from this config when the host is intercepted.',
    // The mitm- name prefix is a convention, not a contract — the
    // dispatcher looks up by host, not name. Prefixing keeps the
    // config out of the way of callable model names so a chat client
    // accidentally requesting "anthropic" doesn't hit a backendless
    // intercept config.
    //
    // pii.patterns is pre-seeded with an empty list so the override
    // editor is visible by default — admins typically want to tighten
    // a couple of pattern actions when intercepting a cloud provider.
    // An empty list serializes out and the redactor ignores it.
    fields: {
      'name': 'mitm-anthropic',
      'mitm.hosts': ['api.anthropic.com'],
      'pii.enabled': true,
      'pii.patterns': [],
    },
  },
]

export default MODEL_TEMPLATES
