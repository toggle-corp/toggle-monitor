package config

import (
	"fmt"
	"time"
)

// DependsOnIntervalWarnings returns human-readable advisories for each
// dependsOn edge where the parent monitor's check interval is LONGER
// than the child's.
//
// Why it matters: dependsOn is evaluated against the parent's
// last-persisted status, so a slow parent widens the window in which a
// child can detect an outage and alert before the parent's own tick
// marks it down (the dependsOn race). Shared parents — a bastion, an
// ingress, a gateway — should therefore carry a SHORT interval so
// push-propagation pauses dependents quickly and the digest collapses
// to the root cause. Advisory only: never blocks startup; the lifecycle
// logs each line at WARN.
func DependsOnIntervalWarnings(cfg Config) []string {
	intervals := make(map[string]time.Duration)
	for _, m := range cfg.Monitors {
		intervals[m.Slug] = m.Interval.AsDuration()
	}
	for _, m := range cfg.SMTPMonitors {
		intervals[m.Slug] = m.Interval.AsDuration()
	}

	var out []string
	check := func(childSlug string, childIv time.Duration, deps []string) {
		// childIv == 0 means "unset/defaulted elsewhere" — skip rather
		// than emit a false positive.
		if childIv == 0 {
			return
		}
		for _, parent := range deps {
			parentIv, ok := intervals[parent]
			if !ok || parentIv == 0 || parentIv <= childIv {
				continue
			}
			out = append(out, fmt.Sprintf(
				"parent %q interval %s is slower than child %q interval %s — give shared dependsOn parents a shorter interval to close the dependsOn race",
				parent, parentIv, childSlug, childIv))
		}
	}
	// Declaration order is deterministic, so no sort is needed.
	for _, m := range cfg.Monitors {
		check(m.Slug, m.Interval.AsDuration(), m.DependsOn)
	}
	for _, m := range cfg.SMTPMonitors {
		check(m.Slug, m.Interval.AsDuration(), m.DependsOn)
	}
	return out
}
