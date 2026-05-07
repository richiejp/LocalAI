package application

import (
	"fmt"
	"path/filepath"

	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/services/cloudproxy/mitm"
	"github.com/mudler/xlog"
)

// defaultInterceptHosts is the allowlist used when the operator
// doesn't supply one. Covers the LLM provider endpoints whose wire
// formats the redactor knows; everything else tunnels.
var defaultInterceptHosts = []string{
	"api.anthropic.com",
	"api.openai.com",
}

func startMITMProxy(app *Application, options *config.ApplicationConfig) error {
	app.mitmMutex.Lock()
	defer app.mitmMutex.Unlock()
	return startMITMLocked(app, options)
}

func startMITMLocked(app *Application, options *config.ApplicationConfig) error {
	caDir := options.MITMCADir
	if caDir == "" {
		base := options.DataPath
		if base == "" {
			base = "."
		}
		caDir = filepath.Join(base, "mitm-ca")
	}

	if app.mitmCA == nil {
		ca, err := mitm.LoadOrCreateCA(caDir)
		if err != nil {
			return fmt.Errorf("ca: %w", err)
		}
		app.mitmCA = ca
	}

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
		CA:             app.mitmCA,
		InterceptHosts: hosts,
		Handler:        handler,
		EventStore:     app.piiEvents,
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

// StopMITM is idempotent.
func (a *Application) StopMITM() error {
	a.mitmMutex.Lock()
	defer a.mitmMutex.Unlock()
	stopMITMLocked(a)
	return nil
}

// RestartMITM reuses the existing CA so trusted clients keep
// working across listener flips.
func (a *Application) RestartMITM() error {
	a.mitmMutex.Lock()
	defer a.mitmMutex.Unlock()
	stopMITMLocked(a)
	if a.applicationConfig.MITMListen == "" {
		xlog.Info("mitm: cloudproxy listener stays disabled (no listen address)")
		return nil
	}
	return startMITMLocked(a, a.applicationConfig)
}

func stopMITMLocked(a *Application) {
	if a.mitmServer == nil {
		return
	}
	a.mitmServer.Stop()
	a.mitmServer = nil
	xlog.Info("mitm: cloudproxy listener stopped")
}
