// Package alertmanager owns the Alertmanager webhook receiver
// pipeline: payload types, match-tree evaluator, Slack rendering,
// rate-limit detector, and the retention sweeper. See ADR-0005.
//
// This file holds only the on-the-wire payload types and their
// envelope-level validation; everything downstream (handler, match,
// blocks, ratelimit) lives in sibling files added in later slices.
package alertmanager

import (
	"errors"
	"fmt"
	"time"
)

// Webhook is the Alertmanager v4 webhook payload envelope. Field tags
// mirror the wire format documented at
// https://prometheus.io/docs/alerting/latest/configuration/#webhook_config.
type Webhook struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts,omitempty"`
	Status            string            `json:"status"` // "firing" | "resolved"
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert is one entry inside Webhook.Alerts. EndsAt is the zero time
// while the alert is firing; AM stamps it at resolve.
type Alert struct {
	Status       string            `json:"status"` // "firing" | "resolved"
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// AlertStatusFiring / AlertStatusResolved are the only per-alert
// status values AM emits in v4. The envelope-level Webhook.Status
// shares the same vocabulary but is derived (resolved iff every
// member alert is resolved).
const (
	AlertStatusFiring   = "firing"
	AlertStatusResolved = "resolved"
)

// Validate enforces the protocol invariants the downstream handler
// will trust: v4-only, at least one alert, every alert has a
// fingerprint + recognized status + non-empty labels. Returning an
// error here is the receiver's only line of defence against a
// misconfigured upstream; the handler turns this into a 400.
func (w *Webhook) Validate() error {
	if w.Version != "4" {
		return fmt.Errorf("alertmanager: unsupported webhook version %q (only v4 is accepted)", w.Version)
	}
	if len(w.Alerts) == 0 {
		return errors.New("alertmanager: webhook carries no alerts")
	}
	for i, a := range w.Alerts {
		if a.Fingerprint == "" {
			return fmt.Errorf("alertmanager: alerts[%d] has empty fingerprint", i)
		}
		if a.Status != AlertStatusFiring && a.Status != AlertStatusResolved {
			return fmt.Errorf("alertmanager: alerts[%d] has unrecognized status %q", i, a.Status)
		}
		if len(a.Labels) == 0 {
			return fmt.Errorf("alertmanager: alerts[%d] has no labels", i)
		}
	}
	return nil
}
