// Package scheduler drives one ticker per monitor with startup jitter
// and in-cycle retries, honoring dependsOn gating and context cancel.
package scheduler
