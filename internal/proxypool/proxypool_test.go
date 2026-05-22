package proxypool_test

import (
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/proxypool"
)

func TestBuild_resolvesValidSocks5Proxy(t *testing.T) {
	p, err := proxypool.Build([]config.Proxy{
		{Slug: "corp", Protocol: "socks5", Server: "proxy.example.com", Port: 1080},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Get("corp") == nil {
		t.Error("expected non-nil dialer for known slug")
	}
	if p.Get("") != nil {
		t.Error("expected nil dialer for empty slug")
	}
	if p.Get("unknown") != nil {
		t.Error("expected nil dialer for unknown slug")
	}
}

func TestBuild_defaultsPortForSocks5(t *testing.T) {
	// Port omitted (0) → defaults to 1080. proxy.SOCKS5 only fails on
	// truly empty addresses, so the dialer should still be built.
	p, err := proxypool.Build([]config.Proxy{
		{Slug: "corp", Protocol: "socks5", Server: "proxy.example.com"},
	})
	if err != nil {
		t.Fatalf("Build with port=0: %v", err)
	}
	if p.Get("corp") == nil {
		t.Error("expected dialer to be built when port defaults")
	}
}

func TestBuild_rejectsUnsupportedProtocol(t *testing.T) {
	_, err := proxypool.Build([]config.Proxy{
		{Slug: "corp", Protocol: "http", Server: "proxy.example.com", Port: 8080},
	})
	if err == nil {
		t.Fatal("expected error for unsupported protocol, got none")
	}
	if !strings.Contains(err.Error(), "unsupported protocol") {
		t.Errorf("expected 'unsupported protocol' in error, got: %v", err)
	}
}

func TestBuild_resolvesPasswordFromEnvVar(t *testing.T) {
	t.Setenv("PROXY_TEST_PASSWORD", "s3cret")
	p, err := proxypool.Build([]config.Proxy{
		{Slug: "corp", Protocol: "socks5", Server: "proxy.example.com", Port: 1080,
			Username: "bot", PasswordEnv: "PROXY_TEST_PASSWORD"},
	})
	if err != nil {
		t.Fatalf("Build with env password: %v", err)
	}
	if p.Get("corp") == nil {
		t.Error("expected dialer to be built with env-resolved password")
	}
}

func TestBuild_rejectsEmptyPasswordEnv(t *testing.T) {
	// PROXY_MISSING_PASSWORD is intentionally not set.
	_, err := proxypool.Build([]config.Proxy{
		{Slug: "corp", Protocol: "socks5", Server: "proxy.example.com", Port: 1080,
			Username: "bot", PasswordEnv: "PROXY_MISSING_PASSWORD"},
	})
	if err == nil {
		t.Fatal("expected error when passwordEnv resolves to empty, got none")
	}
}
