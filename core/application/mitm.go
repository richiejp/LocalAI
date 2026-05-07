package application

import (
	"fmt"
	"path/filepath"

	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/services/cloudproxy/mitm"
	"github.com/mudler/xlog"
)

// defaultInterceptHosts is the allowlist used when the operator
// doesn't pass --mitm-intercept-host. Covers the two LLM provider
// endpoints LocalAI knows the wire format for; everything else
// tunnels (CONNECT pass-through) so the MITM proxy doesn't break
// arbitrary HTTPS traffic that happens to share the listener.
var defaultInterceptHosts = []string{
	"api.anthropic.com",
	"api.openai.com",
}

// startMITMProxy spins up the cloudproxy MITM listener using
// settings from ApplicationConfig. Called from start() when
// MITMListen is non-empty. The CA dir defaults to <data
// path>/mitm-ca if the operator didn't set --mitm-ca-dir, so
// out-of-box the persisted CA lives next to the rest of LocalAI's
// per-installation state.
func startMITMProxy(app *Application, options *config.ApplicationConfig) error {
	caDir := options.MITMCADir
	if caDir == "" {
		base := options.DataPath
		if base == "" {
			base = "."
		}
		caDir = filepath.Join(base, "mitm-ca")
	}

	ca, err := mitm.LoadOrCreateCA(caDir)
	if err != nil {
		return fmt.Errorf("ca: %w", err)
	}
	app.mitmCA = ca

	hosts := options.MITMInterceptHosts
	if len(hosts) == 0 {
		hosts = defaultInterceptHosts
	}

	handler := mitm.NewPIIHandler(mitm.PIIHandlerOptions{
		Redactor:   app.piiRedactor,
		EventStore: app.piiEvents,
	})

	srv, err := mitm.NewServer(mitm.Config{
		Addr:           options.MITMListen,
		CA:             ca,
		InterceptHosts: hosts,
		Handler:        handler,
	})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	app.mitmServer = srv

	xlog.Info("mitm: cloudproxy listener started",
		"addr", srv.Addr(),
		"ca_dir", caDir,
		"intercept_hosts", hosts,
	)
	return nil
}
