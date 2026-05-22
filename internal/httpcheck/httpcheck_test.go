package httpcheck_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/httpcheck"
)

func TestCheck_sendsConfiguredUserAgentAndMethod(t *testing.T) {
	var gotMethod, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	const ua = "toggle-monitor/test"
	res := httpcheck.Check(context.Background(), httpcheck.Config{
		URL:                 srv.URL,
		Method:              "HEAD",
		AcceptedStatusCodes: []int{200},
		Timeout:             2 * time.Second,
		UserAgent:           ua,
	})

	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
	if gotMethod != "HEAD" {
		t.Errorf("server saw method %q, want HEAD", gotMethod)
	}
	if gotUA != ua {
		t.Errorf("server saw User-Agent %q, want %q", gotUA, ua)
	}
}

func TestCheck_followRedirectsFalse_reportsRedirectStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	res := httpcheck.Check(context.Background(), httpcheck.Config{
		URL:                 srv.URL,
		Method:              "GET",
		AcceptedStatusCodes: []int{200},
		Timeout:             2 * time.Second,
		FollowRedirects:     false,
	})

	if res.StatusCode != http.StatusFound {
		t.Errorf("status code: got %d, want 302 (redirect not followed)", res.StatusCode)
	}
	if res.Error == "" {
		t.Errorf("expected error since 302 is not in accepted list, got none")
	}
}

func TestCheck_followRedirectsTrue_followsToFinal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	res := httpcheck.Check(context.Background(), httpcheck.Config{
		URL:                 srv.URL,
		Method:              "GET",
		AcceptedStatusCodes: []int{200},
		Timeout:             2 * time.Second,
		FollowRedirects:     true,
	})

	if res.StatusCode != http.StatusOK {
		t.Errorf("status code: got %d, want 200 (redirect followed)", res.StatusCode)
	}
	if res.Error != "" {
		t.Errorf("expected success, got error: %q", res.Error)
	}
}

func TestCheck_timeout_fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	res := httpcheck.Check(context.Background(), httpcheck.Config{
		URL:                 srv.URL,
		Method:              "GET",
		AcceptedStatusCodes: []int{200},
		Timeout:             20 * time.Millisecond,
	})

	if res.Error == "" {
		t.Fatal("expected error from timeout, got none")
	}
	if res.StatusCode != 0 {
		t.Errorf("status code: got %d, want 0 (no response received)", res.StatusCode)
	}
}

func TestCheck_unexpectedStatusCode_fails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	res := httpcheck.Check(context.Background(), httpcheck.Config{
		URL:                 srv.URL,
		Method:              "GET",
		AcceptedStatusCodes: []int{200},
		Timeout:             2 * time.Second,
	})

	if res.Error == "" {
		t.Fatal("expected error for status 500 not in accepted list, got none")
	}
	if res.StatusCode != 500 {
		t.Errorf("status code: got %d, want 500", res.StatusCode)
	}
}

// TestCheck_TLSInsecureSkipVerify_acceptsSelfSignedCert is the
// motivating case: probing an HTTPS endpoint that presents a
// self-signed cert. Without the flag the TLS handshake fails;
// with it set the probe should complete and report status 200.
func TestCheck_TLSInsecureSkipVerify_acceptsSelfSignedCert(t *testing.T) {
	// httptest.NewTLSServer issues a self-signed cert that's not in
	// any trust store — exactly the scenario this flag exists for.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Baseline: verification ON → handshake fails.
	fail := httpcheck.Check(context.Background(), httpcheck.Config{
		URL:                 srv.URL,
		Method:              "GET",
		AcceptedStatusCodes: []int{200},
		Timeout:             2 * time.Second,
	})
	if fail.Error == "" {
		t.Fatal("expected TLS handshake to fail without TLSInsecureSkipVerify, got no error")
	}

	// With the flag: handshake skipped → probe succeeds.
	ok := httpcheck.Check(context.Background(), httpcheck.Config{
		URL:                   srv.URL,
		Method:                "GET",
		AcceptedStatusCodes:   []int{200},
		Timeout:               2 * time.Second,
		TLSInsecureSkipVerify: true,
	})
	if ok.Error != "" {
		t.Fatalf("expected success with TLSInsecureSkipVerify, got: %q", ok.Error)
	}
	if ok.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", ok.StatusCode)
	}
}

func TestCheck_acceptedStatusCode_succeeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	res := httpcheck.Check(context.Background(), httpcheck.Config{
		URL:                 srv.URL,
		Method:              "GET",
		AcceptedStatusCodes: []int{200},
		Timeout:             2 * time.Second,
	})

	if res.Error != "" {
		t.Fatalf("expected success, got error: %q", res.Error)
	}
	if res.StatusCode != 200 {
		t.Errorf("status code: got %d, want 200", res.StatusCode)
	}
}
