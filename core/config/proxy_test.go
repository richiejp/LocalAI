package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("IsCloudProxy", func() {
	cases := []struct {
		name        string
		backend     string
		upstreamURL string
		want        bool
	}{
		{"local backend, no proxy config", "llama-cpp", "", false},
		{"local backend with stray upstream URL", "llama-cpp", "https://api.openai.com", false},
		{"proxy backend without URL fails closed", "proxy-openai", "", false},
		{"proxy-openai with URL", "proxy-openai", "https://api.openai.com/v1/chat/completions", true},
		{"proxy-anthropic with URL", "proxy-anthropic", "https://api.anthropic.com/v1/messages", true},
		{"proxy-unknown-vendor with URL", "proxy-grok", "https://example.com", true},
	}
	for _, tc := range cases {
		It(tc.name, func() {
			cfg := ModelConfig{
				Backend: tc.backend,
				Proxy:   ProxyConfig{UpstreamURL: tc.upstreamURL},
			}
			Expect(cfg.IsCloudProxy()).To(Equal(tc.want))
		})
	}
})
