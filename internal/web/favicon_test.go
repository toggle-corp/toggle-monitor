package web_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/web"
)

// Browsers fetch /favicon.ico on their own, whatever the <link> tags
// say, so the root path has to answer with the real file — a 404 here
// is one log line per tab.
func TestFavicon_servedFromRootAndStatic(t *testing.T) {
	mux := web.New(nil, nil).Routes()

	cases := []struct {
		path  string
		magic []byte
	}{
		{"/favicon.ico", []byte{0x00, 0x00, 0x01, 0x00}},        // ICONDIR header
		{"/static/favicon.ico", []byte{0x00, 0x00, 0x01, 0x00}}, //
		{"/static/favicon.svg", []byte("<svg")},                 //
		{"/static/apple-touch-icon.png", []byte("\x89PNG\r\n")}, //
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			resp := rr.Result()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d, want 200", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !bytes.HasPrefix(body, tc.magic) {
				t.Errorf("body does not start with the expected file signature (%d bytes read)", len(body))
			}
			if ct := resp.Header.Get("Content-Type"); ct == "" {
				t.Error("no Content-Type on the icon response")
			}
		})
	}
}
