package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

func TestUserMappingValidator_validUserAndSubteam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/users.info"):
			_ = r.ParseForm()
			user := r.PostFormValue("user")
			if user == "U01OK" {
				_, _ = w.Write([]byte(`{"ok": true, "user": {"id":"U01OK","name":"alice"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok": false, "error": "user_not_found"}`))
		case strings.HasSuffix(r.URL.Path, "/usergroups.list"):
			_, _ = w.Write([]byte(`{"ok": true, "usergroups": [{"id":"S01OK","name":"ops"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := slack.NewClient(slack.WithBaseURL(srv.URL))
	v := slack.NewUserMappingValidator(c, map[string]string{
		"alice":    "U01OK",
		"ghost":    "U01BAD",
		"ops-team": "S01OK",
		"missing":  "S99NO",
		"weird":    "X01BAD",
	}, func() string { return "xoxb-test" }, nil)

	v.VerifyOnce(context.Background())

	entries, lastRun := v.Snapshot()
	if lastRun.IsZero() {
		t.Errorf("lastRun should be populated")
	}
	got := map[string]bool{}
	reasons := map[string]string{}
	for _, e := range entries {
		got[e.Slug] = e.OK
		reasons[e.Slug] = e.Reason
	}
	if !got["alice"] {
		t.Errorf("alice should be OK; reason: %q", reasons["alice"])
	}
	if !got["ops-team"] {
		t.Errorf("ops-team should be OK; reason: %q", reasons["ops-team"])
	}
	if got["ghost"] {
		t.Errorf("ghost (U01BAD) should be invalid; reason: %q", reasons["ghost"])
	}
	if got["missing"] {
		t.Errorf("missing (S99NO) should be invalid; reason: %q", reasons["missing"])
	}
	if got["weird"] {
		t.Errorf("weird (X01BAD) should be invalid; reason: %q", reasons["weird"])
	}

	invalid := v.Invalid()
	if len(invalid) != 3 {
		t.Errorf("Invalid() len: got %d, want 3", len(invalid))
	}
}

func TestUserMappingValidator_emptyMappingIsHealthy(t *testing.T) {
	v := slack.NewUserMappingValidator(nil, nil, func() string { return "xoxb" }, nil)
	v.VerifyOnce(context.Background())
	entries, _ := v.Snapshot()
	if len(entries) != 0 {
		t.Errorf("empty mapping should produce no entries, got %d", len(entries))
	}
	if len(v.Invalid()) != 0 {
		t.Error("Invalid() should be empty for empty mapping")
	}
}

func TestUserMappingValidator_noTokenIsNoOp(t *testing.T) {
	c := slack.NewClient(slack.WithBaseURL("http://unused"))
	v := slack.NewUserMappingValidator(c, map[string]string{"alice": "U01OK"},
		func() string { return "" }, nil)
	v.VerifyOnce(context.Background()) // must not panic / hit network
	entries, _ := v.Snapshot()
	if len(entries) != 0 {
		t.Errorf("no-token run should not populate entries, got %d", len(entries))
	}
}

// dump is a small helper so failures are readable.
func dumpEntries(entries []slack.MappingEntryState) string {
	b, _ := json.MarshalIndent(entries, "", "  ")
	return string(b)
}

var _ = dumpEntries
