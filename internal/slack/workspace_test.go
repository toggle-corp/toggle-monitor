package slack_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// teamServer returns an httptest server that responds to auth.test
// with the given team_id-per-token map. An entry with empty team_id
// simulates a transient failure (ok=false).
func teamServer(t *testing.T, teamsByAuth map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/auth.test") {
			http.NotFound(w, r)
			return
		}
		team, ok := teamsByAuth[r.Header.Get("Authorization")]
		if !ok || team == "" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok": false, "error": "invalid_auth"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok": true, "team_id": "` + team + `", "team": "TestCorp"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWorkspaceWatcher_singleWorkspace_healthy(t *testing.T) {
	srv := teamServer(t, map[string]string{
		"Bearer xoxb-a": "T123",
		"Bearer xoxb-b": "T123",
	})
	c := slack.NewClient(slack.WithBaseURL(srv.URL))
	w := slack.NewWorkspaceWatcher(map[string]secret.SecretString{
		"SLACK_TOKEN_A": "xoxb-a",
		"SLACK_TOKEN_B": "xoxb-b",
	}, c, nil)

	if err := w.VerifyOnce(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := w.State()
	if !s.Healthy {
		t.Errorf("expected healthy=true, got state %+v", s)
	}
	if s.TeamID != "T123" {
		t.Errorf("team_id: got %q, want T123", s.TeamID)
	}
}

func TestWorkspaceWatcher_multiWorkspace_returnsErrAndUnhealthy(t *testing.T) {
	srv := teamServer(t, map[string]string{
		"Bearer xoxb-a": "T123",
		"Bearer xoxb-b": "T999",
	})
	c := slack.NewClient(slack.WithBaseURL(srv.URL))
	w := slack.NewWorkspaceWatcher(map[string]secret.SecretString{
		"SLACK_TOKEN_A": "xoxb-a",
		"SLACK_TOKEN_B": "xoxb-b",
	}, c, nil)

	err := w.VerifyOnce(context.Background())
	if !errors.Is(err, slack.ErrMultiWorkspace) {
		t.Fatalf("expected ErrMultiWorkspace, got %v", err)
	}
	if w.State().Healthy {
		t.Errorf("expected unhealthy state after multi-workspace detection")
	}
}

func TestWorkspaceWatcher_transientFailure_returnsNilButUnhealthy(t *testing.T) {
	srv := teamServer(t, map[string]string{
		"Bearer xoxb-a": "T123",
		// Bearer xoxb-b absent → simulates invalid_auth response.
	})
	c := slack.NewClient(slack.WithBaseURL(srv.URL))
	w := slack.NewWorkspaceWatcher(map[string]secret.SecretString{
		"SLACK_TOKEN_A": "xoxb-a",
		"SLACK_TOKEN_B": "xoxb-b",
	}, c, nil)

	if err := w.VerifyOnce(context.Background()); err != nil {
		t.Fatalf("transient failure should NOT block startup, got: %v", err)
	}
	s := w.State()
	if s.Healthy {
		t.Errorf("expected unhealthy after transient failure, got %+v", s)
	}
	if !strings.Contains(s.Error, "invalid_auth") {
		t.Errorf("state error should mention slack error code, got: %q", s.Error)
	}
	if s.TeamID != "T123" {
		t.Errorf("expected to still know team_id from the working token, got %q", s.TeamID)
	}
}

func TestWorkspaceWatcher_zeroTokens_isHealthy(t *testing.T) {
	c := slack.NewClient(slack.WithBaseURL("http://unused"))
	w := slack.NewWorkspaceWatcher(nil, c, nil)
	if err := w.VerifyOnce(context.Background()); err != nil {
		t.Fatalf("zero tokens should succeed: %v", err)
	}
	if !w.State().Healthy {
		t.Error("zero tokens should be considered healthy")
	}
}
