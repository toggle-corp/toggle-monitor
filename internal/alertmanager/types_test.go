package alertmanager_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
)

// firingPayload is a v4 Alertmanager webhook capturing a single firing
// alert. Fields mirror the AM v4 wire format documented at
// https://prometheus.io/docs/alerting/latest/configuration/#webhook_config.
const firingPayload = `{
  "version": "4",
  "groupKey": "{}:{alertname=\"HighCPU\"}",
  "truncatedAlerts": 0,
  "status": "firing",
  "receiver": "toggle_monitor",
  "groupLabels": {"alertname": "HighCPU"},
  "commonLabels": {"alertname": "HighCPU", "severity": "critical"},
  "commonAnnotations": {"summary": "CPU is hot"},
  "externalURL": "https://am.prod.example.test",
  "alerts": [
    {
      "status": "firing",
      "labels": {"alertname": "HighCPU", "severity": "critical", "instance": "pod-1"},
      "annotations": {"summary": "CPU is hot", "runbook_url": "https://runbooks.example.test/cpu"},
      "startsAt": "2026-06-04T10:00:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "https://prom.example.test/graph",
      "fingerprint": "abc123"
    }
  ]
}`

const resolvedPayload = `{
  "version": "4",
  "groupKey": "{}:{alertname=\"HighCPU\"}",
  "status": "resolved",
  "receiver": "toggle_monitor",
  "groupLabels": {"alertname": "HighCPU"},
  "commonLabels": {"alertname": "HighCPU"},
  "commonAnnotations": {},
  "externalURL": "https://am.prod.example.test",
  "alerts": [
    {
      "status": "resolved",
      "labels": {"alertname": "HighCPU", "severity": "critical", "instance": "pod-1"},
      "annotations": {"summary": "CPU is hot"},
      "startsAt": "2026-06-04T10:00:00Z",
      "endsAt":   "2026-06-04T10:15:00Z",
      "generatorURL": "https://prom.example.test/graph",
      "fingerprint": "abc123"
    }
  ]
}`

const mixedPayload = `{
  "version": "4",
  "groupKey": "{}:{alertname=\"Mixed\"}",
  "status": "firing",
  "receiver": "toggle_monitor",
  "groupLabels": {"alertname": "Mixed"},
  "commonLabels": {"alertname": "Mixed"},
  "commonAnnotations": {},
  "externalURL": "https://am.prod.example.test",
  "alerts": [
    {
      "status": "firing",
      "labels": {"alertname": "Mixed", "instance": "pod-1"},
      "annotations": {"summary": "p1"},
      "startsAt": "2026-06-04T10:00:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "",
      "fingerprint": "fp-firing"
    },
    {
      "status": "resolved",
      "labels": {"alertname": "Mixed", "instance": "pod-2"},
      "annotations": {"summary": "p2"},
      "startsAt": "2026-06-04T09:00:00Z",
      "endsAt": "2026-06-04T09:30:00Z",
      "generatorURL": "",
      "fingerprint": "fp-resolved"
    }
  ]
}`

func TestWebhook_unmarshalFiring(t *testing.T) {
	var wh alertmanager.Webhook
	if err := json.Unmarshal([]byte(firingPayload), &wh); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wh.Version != "4" {
		t.Errorf("Version: got %q, want 4", wh.Version)
	}
	if wh.GroupKey != `{}:{alertname="HighCPU"}` {
		t.Errorf("GroupKey: got %q", wh.GroupKey)
	}
	if wh.Status != "firing" {
		t.Errorf("Status: got %q, want firing", wh.Status)
	}
	if wh.Receiver != "toggle_monitor" {
		t.Errorf("Receiver: got %q", wh.Receiver)
	}
	if wh.ExternalURL != "https://am.prod.example.test" {
		t.Errorf("ExternalURL: got %q", wh.ExternalURL)
	}
	if wh.CommonLabels["severity"] != "critical" {
		t.Errorf("CommonLabels[severity]: got %q", wh.CommonLabels["severity"])
	}
	if wh.GroupLabels["alertname"] != "HighCPU" {
		t.Errorf("GroupLabels[alertname]: got %q", wh.GroupLabels["alertname"])
	}
	if wh.CommonAnnotations["summary"] != "CPU is hot" {
		t.Errorf("CommonAnnotations[summary]: got %q", wh.CommonAnnotations["summary"])
	}
	if len(wh.Alerts) != 1 {
		t.Fatalf("len(Alerts): got %d, want 1", len(wh.Alerts))
	}
	a := wh.Alerts[0]
	if a.Status != "firing" {
		t.Errorf("Alert.Status: got %q", a.Status)
	}
	if a.Fingerprint != "abc123" {
		t.Errorf("Fingerprint: got %q", a.Fingerprint)
	}
	if a.Labels["instance"] != "pod-1" {
		t.Errorf("Labels[instance]: got %q", a.Labels["instance"])
	}
	if a.Annotations["runbook_url"] != "https://runbooks.example.test/cpu" {
		t.Errorf("Annotations[runbook_url]: got %q", a.Annotations["runbook_url"])
	}
	wantStart := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	if !a.StartsAt.Equal(wantStart) {
		t.Errorf("StartsAt: got %v, want %v", a.StartsAt, wantStart)
	}
	// Firing alerts have a zero EndsAt.
	if !a.EndsAt.IsZero() {
		t.Errorf("EndsAt: got %v, want zero", a.EndsAt)
	}
	if a.GeneratorURL != "https://prom.example.test/graph" {
		t.Errorf("GeneratorURL: got %q", a.GeneratorURL)
	}
}

func TestWebhook_unmarshalResolved(t *testing.T) {
	var wh alertmanager.Webhook
	if err := json.Unmarshal([]byte(resolvedPayload), &wh); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wh.Status != "resolved" {
		t.Errorf("Status: got %q, want resolved", wh.Status)
	}
	a := wh.Alerts[0]
	if a.Status != "resolved" {
		t.Errorf("Alert.Status: got %q", a.Status)
	}
	wantEnd := time.Date(2026, 6, 4, 10, 15, 0, 0, time.UTC)
	if !a.EndsAt.Equal(wantEnd) {
		t.Errorf("EndsAt: got %v, want %v", a.EndsAt, wantEnd)
	}
}

func TestWebhook_unmarshalMixed(t *testing.T) {
	var wh alertmanager.Webhook
	if err := json.Unmarshal([]byte(mixedPayload), &wh); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wh.Alerts) != 2 {
		t.Fatalf("len(Alerts): got %d, want 2", len(wh.Alerts))
	}
	if wh.Alerts[0].Status != "firing" {
		t.Errorf("Alerts[0].Status: got %q", wh.Alerts[0].Status)
	}
	if wh.Alerts[1].Status != "resolved" {
		t.Errorf("Alerts[1].Status: got %q", wh.Alerts[1].Status)
	}
	if wh.Alerts[0].Fingerprint != "fp-firing" || wh.Alerts[1].Fingerprint != "fp-resolved" {
		t.Errorf("fingerprint roundtrip mismatch: %q / %q",
			wh.Alerts[0].Fingerprint, wh.Alerts[1].Fingerprint)
	}
}

func TestWebhook_Validate_ok(t *testing.T) {
	var wh alertmanager.Webhook
	if err := json.Unmarshal([]byte(firingPayload), &wh); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := wh.Validate(); err != nil {
		t.Errorf("Validate(): unexpected error %v", err)
	}
}

func TestWebhook_Validate_wrongVersion(t *testing.T) {
	wh := alertmanager.Webhook{
		Version: "3",
		Status:  "firing",
		Alerts: []alertmanager.Alert{{
			Status:      "firing",
			Labels:      map[string]string{"alertname": "X"},
			Fingerprint: "fp",
		}},
	}
	err := wh.Validate()
	if err == nil {
		t.Fatal("expected error for version != 4")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error should mention version, got %v", err)
	}
}

func TestWebhook_Validate_noAlerts(t *testing.T) {
	wh := alertmanager.Webhook{Version: "4", Status: "firing"}
	if err := wh.Validate(); err == nil {
		t.Fatal("expected error for empty alerts")
	}
}

func TestWebhook_Validate_emptyFingerprint(t *testing.T) {
	wh := alertmanager.Webhook{
		Version: "4",
		Status:  "firing",
		Alerts: []alertmanager.Alert{{
			Status:      "firing",
			Labels:      map[string]string{"alertname": "X"},
			Fingerprint: "",
		}},
	}
	if err := wh.Validate(); err == nil {
		t.Fatal("expected error for empty fingerprint")
	}
}

func TestWebhook_Validate_unrecognizedAlertStatus(t *testing.T) {
	wh := alertmanager.Webhook{
		Version: "4",
		Status:  "firing",
		Alerts: []alertmanager.Alert{{
			Status:      "weird",
			Labels:      map[string]string{"alertname": "X"},
			Fingerprint: "fp",
		}},
	}
	if err := wh.Validate(); err == nil {
		t.Fatal("expected error for unrecognized alert.status")
	}
}

func TestWebhook_Validate_noLabels(t *testing.T) {
	wh := alertmanager.Webhook{
		Version: "4",
		Status:  "firing",
		Alerts: []alertmanager.Alert{{
			Status:      "firing",
			Labels:      nil,
			Fingerprint: "fp",
		}},
	}
	if err := wh.Validate(); err == nil {
		t.Fatal("expected error for alert with no labels")
	}
}
