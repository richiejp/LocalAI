package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// leafEntry is one cached per-host leaf cert. expiresAt is the time
// after which we re-mint; we keep a healthy buffer so an in-flight
// connection doesn't fail mid-way through a long stream.
type leafEntry struct {
	cert      *tls.Certificate
	expiresAt time.Time
}

// leafLifetime sets how long a minted leaf is considered valid by
// the issuer. Browsers' "max public-trust cert lifetime" (398 days)
// doesn't apply to a private CA, but we keep a moderate window so
// rotation is forced if a key ever leaks.
const leafLifetime = 30 * 24 * time.Hour

// minBeforeReissue is the buffer before expiry at which we re-mint.
// Conservative: an open streaming response shouldn't outlast a leaf,
// and Claude Code sessions can run for hours.
const minBeforeReissue = 24 * time.Hour

// IssueLeaf returns a TLS certificate for the requested host, signed
// by this CA. Calls are deduplicated by host: re-asking for the same
// hostname returns the cached leaf until it nears expiry.
//
// host is the SNI value the client expects (no port). For IP
// addresses we put the IP in the SAN's IPAddresses; for hostnames in
// DNSNames. Wildcards are not auto-expanded — if a client sends SNI
// "api.foo.com" we mint a cert with SAN "api.foo.com", not "*.foo.com".
func (c *CA) IssueLeaf(host string) (*tls.Certificate, error) {
	// Strip a port if the caller passed host:port by mistake. We
	// also lowercase so "API.Anthropic.com" and "api.anthropic.com"
	// share a cache slot.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)

	now := time.Now()

	c.mu.Lock()
	if entry, ok := c.leaves[host]; ok {
		if entry.expiresAt.After(now.Add(minBeforeReissue)) {
			c.mu.Unlock()
			return entry.cert, nil
		}
		// Stale — fall through and reissue.
		delete(c.leaves, host)
	}
	c.mu.Unlock()

	leaf, err := c.mintLeaf(host)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.leaves[host] = &leafEntry{
		cert:      leaf,
		expiresAt: now.Add(leafLifetime),
	}
	c.mu.Unlock()
	return leaf, nil
}

// mintLeaf is the actual cert-issuance path. Pulled out of IssueLeaf
// so the minting work happens outside the lock — a slow ECDSA gen
// shouldn't block other hosts trying to look up their already-cached
// leaves.
func (c *CA) mintLeaf(host string) (*tls.Certificate, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mitm: leaf key for %q: %w", host, err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("mitm: leaf serial: %w", err)
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &leafKey.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("mitm: sign leaf for %q: %w", host, err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{der, c.certDER},
		PrivateKey:  leafKey,
		Leaf:        nil, // tls.Server populates from Certificate[0] on demand
	}, nil
}
