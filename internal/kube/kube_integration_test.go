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

	// Remove "goes" and re-reconcile.
	lister.Items = lister.Items[:1]
	// Sleep briefly so the second reconcile's "before" cutoff is past
	// the first reconcile's last_seen_at timestamps. (Could also use
	// WithNow to inject a clock.)
	time.Sleep(50 * time.Millisecond)
	if err := w.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = repo.ListDiscoverySnapshot(ctx)
	if len(rows) != 1 || rows[0].IngressName != "stays" {
		t.Errorf("after pruning: got %+v, want only 'stays'", rows)
	}
}
