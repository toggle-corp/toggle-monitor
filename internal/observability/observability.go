// Package observability registers Prometheus metrics and configures
// the slog JSON logger.
package observability

import (
	// Prometheus client + promhttp are locked here; the /metrics
	// handler and series definitions land in Issue 14.
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/prometheus/client_golang/prometheus/promhttp"
)
