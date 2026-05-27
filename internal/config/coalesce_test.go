package config

import (
	"strings"
	"testing"
	"time"
)

func dur(d time.Duration) Duration { return Duration(d) }

func TestDependsOnIntervalWarnings(t *testing.T) {
	cfg := Config{
		Monitors: []Monitor{
			{Slug: "bastion", Interval: dur(10 * time.Minute)}, // slow shared parent
			{Slug: "api", Interval: dur(1 * time.Minute), DependsOn: []string{"bastion"}},
			{Slug: "fast-parent", Interval: dur(30 * time.Second)},
			{Slug: "web", Interval: dur(1 * time.Minute), DependsOn: []string{"fast-parent"}}, // OK
		},
	}
	warns := DependsOnIntervalWarnings(cfg)
	if len(warns) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], "bastion") || !strings.Contains(warns[0], "api") {
		t.Fatalf("warning should name the slow parent + child: %s", warns[0])
	}
}

func TestDependsOnIntervalWarnings_noneWhenParentFaster(t *testing.T) {
	cfg := Config{
		Monitors: []Monitor{
			{Slug: "bastion", Interval: dur(30 * time.Second)},
			{Slug: "api", Interval: dur(5 * time.Minute), DependsOn: []string{"bastion"}},
		},
	}
	if w := DependsOnIntervalWarnings(cfg); len(w) != 0 {
		t.Fatalf("fast parent should produce no warnings, got %v", w)
	}
}

func TestDependsOnIntervalWarnings_skipsUnsetIntervals(t *testing.T) {
	cfg := Config{
		Monitors: []Monitor{
			{Slug: "p"}, // interval unset
			{Slug: "c", DependsOn: []string{"p"}},
		},
	}
	if w := DependsOnIntervalWarnings(cfg); len(w) != 0 {
		t.Fatalf("unset intervals should be skipped, got %v", w)
	}
}
