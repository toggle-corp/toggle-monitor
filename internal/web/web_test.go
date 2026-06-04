package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/web"
)

// TestRegisterRoute_installsHandlerOnMux verifies that a route
// registered via Server.RegisterRoute before Routes() is reachable
// through the returned mux. Guards the seam lifecycle wiring uses to
// install the Alertmanager webhook handler without web.Server
// reaching back into internal/alertmanager.
func TestRegisterRoute_installsHandlerOnMux(t *testing.T) {
	srv := web.New(nil, nil)

	var hits atomic.Int32
	srv.RegisterRoute("POST /webhooks/alertmanager", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ok"))
	}))

	mux := srv.Routes()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/alertmanager", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	resp := rr.Result()
	body, _ := io.ReadAll(resp.Body)

	if hits.Load() != 1 {
		t.Fatalf("handler hit count: got %d, want 1", hits.Load())
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	if string(body) != "ok" {
		t.Errorf("body: got %q, want %q", string(body), "ok")
	}
}

// TestRegisterRoute_methodGateRejectsWrongMethod confirms that the
// "POST /…" pattern syntax actually method-gates the registered
// handler (i.e. RegisterRoute hands off to mux.Handle with the full
// go-1.22 pattern syntax, not just the path).
func TestRegisterRoute_methodGateRejectsWrongMethod(t *testing.T) {
	srv := web.New(nil, nil)

	srv.RegisterRoute("POST /webhooks/alertmanager", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called on a GET")
	}))

	req := httptest.NewRequest(http.MethodGet, "/webhooks/alertmanager", nil)
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)

	if rr.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on POST-only route: got %d, want 405", rr.Result().StatusCode)
	}
}
