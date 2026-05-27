//go:build integration

package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/kube"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// fakeLister returns the configured ingresses on each List(); tests
// reassign Items to evolve the cluster state between reconciles.
type fakeLister struct {
	Items []*networkingv1.Ingress
}

func (f *fakeLister) List() ([]*networkingv1.Ingress, error) {
	return f.Items, nil
}

// Get matches the production IngressLister contract so the fake stays
// substitutable. Tests that drive Get directly populate Items first.
func (f *fakeLister) Get(namespace, name string) (*networkingv1.Ingress, error) {
	for _, it := range f.Items {
		if it.Namespace == namespace && it.Name == name {
			return it, nil
		}
	}
	return nil, kube.ErrIngressNotFound
}

func ingress(ns, name string, hosts ...string) *networkingv1.Ingress {
	rules := make([]networkingv1.IngressRule, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, networkingv1.IngressRule{Host: h})
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       networkingv1.IngressSpec{Rules: rules},
	}
}

func newRepo(t *testing.T) *store.Repo {
	t.Helper()
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

func TestWatcher_observeOnly_recordsKubeInvalidForEveryHost(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	lister := &fakeLister{
		Items: []*networkingv1.Ingress{
			ingress("ns-a", "api", "api.example.com"),
			ingress("ns-a", "multi", "foo.example.com", "bar.example.com"),
		},
	}
	w := kube.New(repo, lister, kube.Options{
		ResyncInterval: time.Minute,
	})
	if err := w.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	rows, err := repo.ListDiscoverySnapshot(ctx)
	if err != nil {
		t.Fatalf("list snapshot: %v", err)
	}
	if got, want := len(rows), 3; got != want {
		t.Fatalf("snapshot rows: got %d, want %d", got, want)
	}
	for _, row := range rows {
		if row.Status != "kube-invalid" {
			t.Errorf("status: got %q for %s/%s/%s, want kube-invalid", row.Status, row.Namespace, row.IngressName, row.Host)
		}
		if row.Reason == nil || *row.Reason != "no materializer configured" {
			t.Errorf("reason: got %v for %s/%s/%s, want 'no materializer configured'", row.Reason, row.Namespace, row.IngressName, row.Host)
		}
	}
}

// TestWatcher_reconcile_clockSkewDoesNotPrunePresentIngresses is a
// regression test for the prune bug where the reconcile pass compared
// Go-time (startedAt) against Postgres-time (last_seen_at). Normal NTP
// drift between the toggle-monitor pod and the Postgres pod (tens of
// ms) was enough to make the earliest upserts in a pass land with
// last_seen_at < startedAt and get pruned in the same pass — soft-
// deleting monitors whose ingresses were still in the cluster.
//
// We simulate the worst-case skew by injecting a Now() that puts the
// watcher's clock 24h ahead of the Postgres clock. With the old
// timestamp-watermark prune this would delete every just-upserted row;
// with the observed-set prune it must be a no-op because every listed
// ingress was observed this pass.
func TestWatcher_reconcile_clockSkewDoesNotPrunePresentIngresses(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	lister := &fakeLister{
		Items: []*networkingv1.Ingress{
			ingress("ns-a", "ing1", "h1.example.com"),
			ingress("ns-a", "ing2", "h2.example.com"),
			ingress("ns-b", "ing3", "h3.example.com"),
		},
	}

	future := time.Now().Add(24 * time.Hour)
	w := kube.New(repo, lister, kube.Options{
		ResyncInterval: time.Minute,
		Now:            func() time.Time { return future },
	})

	if err := w.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	rows, err := repo.ListDiscoverySnapshot(ctx)
	if err != nil {
		t.Fatalf("list snapshot: %v", err)
	}
	if got := len(rows); got != 3 {
		t.Errorf("after reconcile with skewed clock: got %d rows, want 3 (every listed ingress should survive prune regardless of clock state)", got)
	}
}

func TestWatcher_reconcile_prunesDisappearedIngresses(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	lister := &fakeLister{
		Items: []*networkingv1.Ingress{
			ingress("ns-a", "stays", "stays.example.com"),
			ingress("ns-a", "goes", "goes.example.com"),
		},
	}
	w := kube.New(repo, lister, kube.Options{ResyncInterval: time.Minute})

	if err := w.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := repo.ListDiscoverySnapshot(ctx)
	if len(rows) != 2 {
		t.Fatalf("after first reconcile: got %d rows, want 2", len(rows))
	}

	// Remove "goes" and re-reconcile. The observed-set prune is
	// clock-independent so no time.Sleep is needed between reconciles.
	lister.Items = lister.Items[:1]
	if err := w.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = repo.ListDiscoverySnapshot(ctx)
	if len(rows) != 1 || rows[0].IngressName != "stays" {
		t.Errorf("after pruning: got %+v, want only 'stays'", rows)
	}
}

// fakeMaterializer lets a test decide, per (ingress, host), what
// snapshot row the watcher should persist — standing in for the real
// merger so this test can isolate the watcher's teardown wiring.
type fakeMaterializer struct {
	fn func(ctx context.Context, ing *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error)
}

func (f *fakeMaterializer) Materialize(ctx context.Context, ing *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error) {
	return f.fn(ctx, ing, host)
}

// recordingSink captures every removal call and performs the real
// store-side teardown (soft-delete) the production sink does, so the
// test can assert the open incident is resolved through the real DB.
type recordingSink struct {
	repo  *store.Repo
	calls []removalCall
}

type removalCall struct {
	slug   string
	reason string
}

func (s *recordingSink) OnKubeMonitorRemoved(ctx context.Context, slug, reason string) {
	s.calls = append(s.calls, removalCall{slug: slug, reason: reason})
	_ = s.repo.SoftDeleteMonitor(ctx, slug, reason)
}

// TestWatcher_addedToInvalidFlip_tearsDownMonitorAndIncident covers the
// real-world cleanup: a tuple that previously materialized a monitor
// (and is sitting on an open incident) flips to kube-invalid while the
// ingress stays in the cluster. The watcher must route the vacated slug
// through the removal sink so the orphaned monitor + its incident are
// torn down — even though the observed-set prune never fires (the tuple
// is still present).
func TestWatcher_addedToInvalidFlip_tearsDownMonitorAndIncident(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	const slug = "acme-wildcard-foo-example-test"
	const ns, name, host = "acme", "wildcard", "*.foo.example.test"

	// Simulate the pre-fix state: a materialized kube monitor, currently
	// DOWN with an open incident, plus its `added` snapshot row.
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: slug, FriendlyName: "wildcard", URL: "https://" + host + "/",
		Kind: store.KindHTTP, Source: store.SourceKube,
	}); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	t0 := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	if err := repo.ApplyCheck(ctx, slug,
		alert.State{Status: alert.StatusDown, OpenedAt: t0},
		t0, 0, "dial tcp: lookup *.foo.example.test: no such host",
		&alert.Event{Type: alert.EventOpen, At: t0, Error: "no such host"},
	); err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	seedSlug := slug
	if _, err := repo.UpsertDiscoverySnapshot(ctx, store.DiscoverySnapshotRow{
		Namespace: ns, IngressName: name, Host: host,
		Status: "added", MonitorSlug: &seedSlug,
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if n, _ := repo.CountOpenIncidents(ctx); n != 1 {
		t.Fatalf("precondition: open incidents = %d, want 1", n)
	}

	// Fixed-code materializer: the wildcard host now resolves to
	// kube-invalid with no monitor slug.
	mat := &fakeMaterializer{fn: func(_ context.Context, ing *networkingv1.Ingress, h string) (store.DiscoverySnapshotRow, error) {
		reason := "kube-invalid: wildcard host not probeable"
		return store.DiscoverySnapshotRow{
			Namespace: ing.Namespace, IngressName: ing.Name, Host: h,
			Status: "kube-invalid", Reason: &reason, MonitorSlug: nil,
		}, nil
	}}
	sink := &recordingSink{repo: repo}
	lister := &fakeLister{Items: []*networkingv1.Ingress{ingress(ns, name, host)}}
	w := kube.New(repo, lister, kube.Options{
		ResyncInterval: time.Minute,
		Materializer:   mat,
		RemovalSink:    sink,
	})

	if err := w.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(sink.calls) != 1 {
		t.Fatalf("removal sink: got %d calls, want exactly 1 (%+v)", len(sink.calls), sink.calls)
	}
	if sink.calls[0].slug != slug {
		t.Errorf("removed slug: got %q, want %q", sink.calls[0].slug, slug)
	}
	// The ingress is alive — the reason must NOT claim it was removed.
	if r := sink.calls[0].reason; strings.Contains(r, "ingress removed") || !strings.Contains(r, "invalid") {
		t.Errorf("removal reason should reflect the host going invalid (not 'ingress removed'), got %q", r)
	}
	if n, _ := repo.CountOpenIncidents(ctx); n != 0 {
		t.Errorf("open incidents after teardown: got %d, want 0", n)
	}
	got, err := repo.GetMonitor(ctx, slug)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if !got.Archived {
		t.Errorf("monitor should be archived after teardown")
	}
}

// TestWatcher_transientMaterializeError_doesNotTearDown is the
// regression guard for the bug that sank the plan-set-keyed design: a
// transient Materialize error on a healthy, still-present ingress must
// NOT be mistaken for a removal. The watcher skips the upsert on error,
// so no slug is vacated and the monitor + its incident stay intact.
func TestWatcher_transientMaterializeError_doesNotTearDown(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	const slug = "acme-api-example-test"
	const ns, name, host = "acme", "api", "api.example.test"

	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: slug, FriendlyName: "api", URL: "https://" + host + "/",
		Kind: store.KindHTTP, Source: store.SourceKube,
	}); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	t0 := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	if err := repo.ApplyCheck(ctx, slug,
		alert.State{Status: alert.StatusDown, OpenedAt: t0},
		t0, 503, "down",
		&alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 503, Error: "down"},
	); err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	seedSlug := slug
	if _, err := repo.UpsertDiscoverySnapshot(ctx, store.DiscoverySnapshotRow{
		Namespace: ns, IngressName: name, Host: host,
		Status: "added", MonitorSlug: &seedSlug,
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	mat := &fakeMaterializer{fn: func(_ context.Context, _ *networkingv1.Ingress, _ string) (store.DiscoverySnapshotRow, error) {
		return store.DiscoverySnapshotRow{}, errors.New("transient: apiserver hiccup")
	}}
	sink := &recordingSink{repo: repo}
	lister := &fakeLister{Items: []*networkingv1.Ingress{ingress(ns, name, host)}}
	w := kube.New(repo, lister, kube.Options{
		ResyncInterval: time.Minute,
		Materializer:   mat,
		RemovalSink:    sink,
	})

	if err := w.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(sink.calls) != 0 {
		t.Fatalf("transient error must NOT trigger removal, got %d calls: %+v", len(sink.calls), sink.calls)
	}
	if n, _ := repo.CountOpenIncidents(ctx); n != 1 {
		t.Errorf("incident must survive a transient error: got %d open, want 1", n)
	}
	got, err := repo.GetMonitor(ctx, slug)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if got.Archived {
		t.Errorf("monitor must NOT be archived after a transient materialize error")
	}
}
