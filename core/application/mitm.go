package application

import (
	"fmt"
	"path/filepath"
	"sync"

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

// mitmMutex serialises start/stop/restart so a runtime settings
// flip can't race with another flip mid-flight.
var mitmMutex sync.Mutex

func startMITMProxy(app *Application, options *config.ApplicationConfig) error {
	mitmMutex.Lock()
	defer mitmMutex.Unlock()
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
	mitmMutex.Lock()
	defer mitmMutex.Unlock()
	if a.mitmServer == nil {
		return nil
	}
	a.mitmServer.Stop()
	a.mitmServer = nil
	xlog.Info("mitm: cloudproxy listener stopped")
	return nil
}

// RestartMITM stops the running listener (if any) and starts a new
// one against current ApplicationConfig. Used by /api/settings to
// pick up MITMListen / MITMInterceptHosts changes without a process
// restart. The CA is reused across restarts so trusted clients keep
// working.
func (a *Application) RestartMITM() error {
	mitmMutex.Lock()
	defer mitmMutex.Unlock()
	if a.mitmServer != nil {
		a.mitmServer.Stop()
		a.mitmServer = nil
	}
	if a.applicationConfig.MITMListen == "" {
		xlog.Info("mitm: cloudproxy listener stays disabled (no listen address)")
		return nil
	}
	return startMITMLocked(a, a.applicationConfig)
}
