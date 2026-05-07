// Package mitm implements a TLS man-in-the-middle proxy so LocalAI
// can apply per-request PII redaction to traffic from clients like
// Claude Code and OpenAI Codex CLI that authenticate via OAuth /
// subscription rather than via API keys held by LocalAI.
//
// The proxy is wire-format-faithful at the network layer: clients
// configure HTTPS_PROXY=http://localai:port, send a CONNECT, and
// the proxy either tunnels the bytes (default for unknown hosts) or
// terminates TLS using a per-host leaf certificate signed by a
// LocalAI-owned CA, parses the plaintext HTTP request, applies PII
// redaction on known LLM API endpoints, and re-encrypts to the real
// upstream. Hosts the proxy doesn't intercept pass through TCP-only
// — OAuth flows, telemetry, and arbitrary HTTPS keep working
// without a CA-trust install.
//
// CA distribution is the operational tax: clients have to trust the
// CA cert this package generates. The package exposes the cert as a
// single-file PEM at LoadOrCreateCA().PublicCertPEM() so the admin
// can route it through `NODE_EXTRA_CA_CERTS` for Node-based CLIs
// (Claude Code, Codex), the system trust store, or a Hugo-style
// docs link served from the LocalAI HTTP API.
package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA is the LocalAI-owned certificate authority used to sign leaf
// certs for intercepted hosts. The CA private key never leaves the
// process — it stays in memory plus the on-disk PEM file with mode
// 0600. Leaf certs are minted on demand and cached in-memory; they
// are ephemeral, never written to disk.
//
// Lifetime: the CA is generated once on first start and persisted.
// Restarting LocalAI loads the same CA so clients that already
// trust it keep working. There's no rotation in the MVP — operators
// who need to rotate delete the PEM files and reinstall the cert
// on every client.
type CA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *ecdsa.PrivateKey

	// publicPEM is the CA cert encoded as PEM, ready to serve from
	// the admin endpoint or hand to a client via curl. Cached so we
	// don't re-encode on every download request.
	publicPEM []byte

	// mu guards the leaf-cert cache below. Mints are rare (one per
	// distinct hostname per process lifetime) and short, so a plain
	// Mutex is simpler than syncing.Map without giving up much.
	mu     sync.Mutex
	leaves map[string]*leafEntry // hostname → cached leaf
}

// LoadOrCreateCA loads the CA from dir if both files exist, or
// generates a new ECDSA-P256 CA and persists it. dir is created with
// mode 0700 if it does not exist. The private-key file is mode 0600;
// the public cert is mode 0644 (it's safe to read — that's the whole
// point of distributing it).
//
// This function is safe to call once at startup, on a single process.
// Concurrent calls from multiple processes against the same dir is
// not supported (no lock file); operators should not point two
// LocalAI instances at the same CA dir without external coordination.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mitm: create ca dir %q: %w", dir, err)
	}

	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	certPEM, err1 := os.ReadFile(certPath)
	keyPEM, err2 := os.ReadFile(keyPath)
	if err1 == nil && err2 == nil {
		ca, err := parseCA(certPEM, keyPEM)
		if err == nil {
			return ca, nil
		}
		// Fall through and regenerate. We don't auto-delete the
		// existing files — the operator might have hand-edited
		// them. Surface the parse error instead.
		return nil, fmt.Errorf("mitm: parse existing CA at %s: %w (delete to regenerate)", dir, err)
	}

	ca, certPEMOut, keyPEMOut, err := generateCA()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEMOut, 0o644); err != nil {
		return nil, fmt.Errorf("mitm: write ca cert %q: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEMOut, 0o600); err != nil {
		return nil, fmt.Errorf("mitm: write ca key %q: %w", keyPath, err)
	}
	return ca, nil
}

// generateCA mints a fresh CA. Split out from LoadOrCreateCA so
// tests can spin up a CA without touching disk (NewInMemoryCA below
// is the test-only constructor).
func generateCA() (*CA, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mitm: generate ca key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mitm: serial: %w", err)
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "LocalAI MITM Proxy CA",
			Organization: []string{"LocalAI"},
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // can only sign leaves, not other CAs
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mitm: create ca cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mitm: re-parse ca cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mitm: marshal ca key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return &CA{
		cert:      cert,
		certDER:   der,
		key:       key,
		publicPEM: certPEM,
		leaves:    make(map[string]*leafEntry),
	}, certPEM, keyPEM, nil
}

// NewInMemoryCA mints an ephemeral CA for tests. The cert + key live
// only in the returned struct; nothing is written to disk.
func NewInMemoryCA() (*CA, error) {
	ca, _, _, err := generateCA()
	return ca, err
}

// parseCA decodes a previously persisted CA from PEM. Used on
// startup when the CA dir already holds files from a prior run.
func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("mitm: ca cert PEM block missing or wrong type")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse ca cert: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("mitm: stored cert at is not a CA")
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("mitm: ca key PEM block missing")
	}
	var key *ecdsa.PrivateKey
	switch keyBlock.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("mitm: parse ec ca key: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("mitm: parse pkcs8 ca key: %w", err)
		}
		ecKey, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("mitm: pkcs8 key is not ECDSA")
		}
		key = ecKey
	default:
		return nil, fmt.Errorf("mitm: unsupported ca key PEM type %q", keyBlock.Type)
	}

	return &CA{
		cert:      cert,
		certDER:   certBlock.Bytes,
		key:       key,
		publicPEM: certPEM,
		leaves:    make(map[string]*leafEntry),
	}, nil
}

// PublicCertPEM returns the PEM-encoded CA certificate for clients
// to install in their trust store. Safe to expose unauthenticated —
// the cert is the public half; an adversary already needs the
// private key to forge anything with it, and that key never leaves
// disk.
func (c *CA) PublicCertPEM() []byte {
	// Return a copy so callers can't mutate the cached buffer. The
	// PEM is small (< 1 KiB) so the alloc cost is irrelevant.
	out := make([]byte, len(c.publicPEM))
	copy(out, c.publicPEM)
	return out
}

// Cert returns the parsed CA certificate. Used internally by leaf
// minting; exposed for tests that want to validate the leaf chains
// up to the CA.
func (c *CA) Cert() *x509.Certificate { return c.cert }
