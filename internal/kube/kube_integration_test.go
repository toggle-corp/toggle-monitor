//go:build integration

package kube_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
