package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/toggle-corp/toggle-monitor/internal/observability"
)

// The /issues page is the operator's misconfiguration surface, but it
// only exists while someone is looking at it. Exporting the same counts
// as a gauge is what makes them alertable through Prometheus →
// Alertmanager.

func TestIssuesReporter_publishesOneSeriesPerSource(t *testing.T) {
	m := observability.New()
	r := &issuesReporter{
		metrics: m,
		counts: map[string]func(context.Context) (int, bool){
			issueSourceKubeInvalid:   func(context.Context) (int, bool) { return 2, true },
			issueSourceSlackMapping:  func(context.Context) (int, bool) { return 0, true },
			issueSourceMissingParent: func(context.Context) (int, bool) { return 1, true },
			issueSourceAnnotation:    func(context.Context) (int, bool) { return 3, true },
		},
	}

	r.refresh(context.Background())

	want := `
# HELP toggle_monitor_issues Operator-actionable issues currently detected, partitioned by source. Mirrors the /issues page.
# TYPE toggle_monitor_issues gauge
toggle_monitor_issues{source="annotation"} 3
toggle_monitor_issues{source="kube-invalid"} 2
toggle_monitor_issues{source="missing-parent"} 1
toggle_monitor_issues{source="slack-mapping"} 0
`
	if err := testutil.CollectAndCompare(m.Issues, strings.NewReader(want), "toggle_monitor_issues"); err != nil {
		t.Error(err)
	}
}

// A source that drops back to zero must keep emitting its series —
// otherwise the alert's expression loses the series entirely and the
// alert never resolves.
func TestIssuesReporter_zeroIsPublishedNotDropped(t *testing.T) {
	m := observability.New()
	n := 4
	r := &issuesReporter{
		metrics: m,
		counts: map[string]func(context.Context) (int, bool){
			issueSourceAnnotation: func(context.Context) (int, bool) { return n, true },
		},
	}
	r.refresh(context.Background())
	n = 0
	r.refresh(context.Background())

	if got := testutil.ToFloat64(m.Issues.WithLabelValues(issueSourceAnnotation)); got != 0 {
		t.Errorf("gauge = %v, want 0", got)
	}
	if got := testutil.CollectAndCount(m.Issues, "toggle_monitor_issues"); got != 1 {
		t.Errorf("series count = %d, want the zeroed series to remain", got)
	}
}

// A source that cannot read its input must leave its series untouched.
// Writing a zero would resolve a real alert on a transient DB error.
func TestIssuesReporter_unreadableSourceHoldsItsLastValue(t *testing.T) {
	m := observability.New()
	readable := true
	r := &issuesReporter{
		metrics: m,
		counts: map[string]func(context.Context) (int, bool){
			issueSourceKubeInvalid: func(context.Context) (int, bool) {
				if !readable {
					return 0, false
				}
				return 5, true
			},
		},
	}
	r.refresh(context.Background())
	readable = false
	r.refresh(context.Background())

	if got := testutil.ToFloat64(m.Issues.WithLabelValues(issueSourceKubeInvalid)); got != 5 {
		t.Errorf("gauge = %v, want the held value 5", got)
	}
}
