// Package proxypool resolves the YAML `proxies:` block into ready-to-use
// proxy dialers, keyed by slug. The scheduler looks up the right
// dialer for each monitor and hands it to the HTTP probe.
package proxypool

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"golang.org/x/net/proxy"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
)

// defaultPorts holds the per-protocol fallback for proxies whose
// `port` field is omitted.
var defaultPorts = map[string]int{
	"socks5": 1080,
}

// Pool holds the resolved dialer for every configured proxy. Built
// once at startup; the scheduler reads from it on every plan refresh.
type Pool struct {
	mu      sync.RWMutex
	dialers map[string]proxy.Dialer
	// secrets keeps the resolved passwords behind secret.SecretString
	// so they don't accidentally land in logs.
	secrets map[string]secret.SecretString
}

// Build resolves every entry in `proxies` into a dialer. Returns the
// first error encountered (env var unset, address parse failure, etc.)
// so the operator gets a clean startup failure rather than runtime
// surprises mid-tick.
func Build(proxies []config.Proxy) (*Pool, error) {
	p := &Pool{
		dialers: make(map[string]proxy.Dialer, len(proxies)),
		secrets: make(map[string]secret.SecretString, len(proxies)),
	}
	for _, cfg := range proxies {
		dialer, pw, err := buildOne(cfg)
		if err != nil {
			return nil, fmt.Errorf("proxy %q: %w", cfg.Slug, err)
		}
		p.dialers[cfg.Slug] = dialer
		p.secrets[cfg.Slug] = pw
	}
	return p, nil
}

// Get returns the dialer for the given slug, or nil if the slug is
// empty / unknown. Callers should treat nil as "no proxy" (direct
// dial) rather than an error: config-load validation already rejects
// unknown references, so nil at runtime means the monitor doesn't
// declare a proxy.
func (p *Pool) Get(slug string) proxy.Dialer {
	if p == nil || slug == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dialers[slug]
}

func buildOne(cfg config.Proxy) (proxy.Dialer, secret.SecretString, error) {
	if cfg.Protocol != "socks5" {
		return nil, "", fmt.Errorf("unsupported protocol %q", cfg.Protocol)
	}
	port := cfg.Port
	if port == 0 {
		port = defaultPorts[cfg.Protocol]
	}
	if port == 0 {
		return nil, "", errors.New("port is required and protocol has no default")
	}

	var auth *proxy.Auth
	var pw secret.SecretString
	if cfg.Username != "" {
		auth = &proxy.Auth{User: cfg.Username}
		if cfg.PasswordEnv != "" {
			raw := os.Getenv(cfg.PasswordEnv)
			if raw == "" {
				return nil, "", fmt.Errorf("env var %q is empty", cfg.PasswordEnv)
			}
			pw = secret.SecretString(raw)
			auth.Password = raw
		}
	}

	addr := net.JoinHostPort(cfg.Server, strconv.Itoa(port))
	d, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
	if err != nil {
		return nil, "", fmt.Errorf("socks5 setup: %w", err)
	}
	return d, pw, nil
}
