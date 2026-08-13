package kube_test

import (
	"context"
	"sync"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toggle-corp/toggle-monitor/internal/kube"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// countingLister is a concurrency-safe IngressLister that records how
// many times the watcher listed the cluster. List count is the
// observable proxy for "a reconcile pass ran", which is what the nudge
// path is specified in terms of.
type countingLister struct {
	mu    sync.Mutex
	items []*networkingv1.Ingress
	lists int
}

func (l *countingLister) List() ([]*networkingv1.Ingress, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lists++
	out := make([]*networkingv1.Ingress, len(l.items))
	copy(out, l.items)
	return out, nil
}

func (l *countingLister) Get(namespace, name string) (*networkingv1.Ingress, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, it := range l.items {
		if it.Namespace == namespace && it.Name == name {
			return it, nil
		}
	}
	return nil, kube.ErrIngressNotFound
}

func (l *countingLister) setItems(items ...*networkingv1.Ingress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = items
}

func (l *countingLister) listCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lists
}

// memStore is an in-memory SnapshotStore so the nudge/debounce tests
// run without Postgres. It records the tuples it was told about and
// reports the slugs a prune pass dropped, which is all the watcher's
// removal routing reads.
type memStore struct {
	mu   sync.Mutex
	rows map[store.DiscoverySnapshotKey]string // tuple → monitor slug ("" when unmaterialized)
}

func newMemStore() *memStore {
	return &memStore{rows: map[store.DiscoverySnapshotKey]string{}}
}

func (s *memStore) UpsertDiscoverySnapshot(_ context.Context, row store.DiscoverySnapshotRow) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := store.DiscoverySnapshotKey{Namespace: row.Namespace, IngressName: row.IngressName, Host: row.Host}
	var slug string
	if row.MonitorSlug != nil {
		slug = *row.MonitorSlug
	}
	vacated := ""
	if prev, ok := s.rows[key]; ok && prev != "" && prev != slug {
		vacated = prev
	}
	s.rows[key] = slug
	return vacated, nil
}

func (s *memStore) PruneDiscoverySnapshotExcept(_ context.Context, observed []store.DiscoverySnapshotKey) (int64, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(observed) == 0 {
		return 0, nil, nil
	}
	keep := make(map[store.DiscoverySnapshotKey]struct{}, len(observed))
	for _, k := range observed {
		keep[k] = struct{}{}
	}
	var pruned int64
	var slugs []string
	for key, slug := range s.rows {
		if _, ok := keep[key]; ok {
			continue
		}
		delete(s.rows, key)
		pruned++
		if slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return pruned, slugs, nil
}

func ing(ns, name string, hosts ...string) *networkingv1.Ingress {
	rules := make([]networkingv1.IngressRule, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, networkingv1.IngressRule{Host: h})
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       networkingv1.IngressSpec{Rules: rules},
	}
}

// waitForLists blocks until the lister has been called at least n times
// or the deadline passes, returning the final count.
func waitForLists(t *testing.T, l *countingLister, n int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if got := l.listCount(); got >= n {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	return l.listCount()
}

// TestWatcher_nudge_reconcilesBeforeTicker is the behaviour the whole
// change exists for: a cluster event must reach the reconcile pass
// without waiting out resyncInterval, so removal teardown lands before
// the dispatcher's pending window elapses.
func TestWatcher_nudge_reconcilesBeforeTicker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lister := &countingLister{items: []*networkingv1.Ingress{ing("ns", "api", "api.example.test")}}
	w := kube.New(newMemStore(), lister, kube.Options{
		ResyncInterval: time.Hour, // ticker must never be the reason a pass runs
		WatchDebounce:  20 * time.Millisecond,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	if got := waitForLists(t, lister, 1, time.Second); got != 1 {
		t.Fatalf("startup reconcile: got %d lists, want 1", got)
	}

	w.Nudge()

	if got := waitForLists(t, lister, 2, time.Second); got < 2 {
		t.Errorf("nudge should trigger a reconcile within the debounce window: got %d lists, want >= 2", got)
	}
	cancel()
	<-done
}

// slugMaterializer materializes every host into an `added` row whose
// monitor slug is the host, so a pruned tuple has a slug to route
// through the removal sink.
type slugMaterializer struct{}

func (slugMaterializer) Materialize(_ context.Context, i *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error) {
	slug := host
	return store.DiscoverySnapshotRow{
		Namespace: i.Namespace, IngressName: i.Name, Host: host,
		Status: "added", MonitorSlug: &slug,
	}, nil
}

// collectingSink records removal calls without touching a database.
type collectingSink struct {
	mu    sync.Mutex
	slugs []string
}

func (s *collectingSink) OnKubeMonitorRemoved(_ context.Context, slug, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slugs = append(s.slugs, slug)
}

func (s *collectingSink) removed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.slugs...)
}

// TestWatcher_nudge_tearsDownRemovedIngress is the end-to-end shape of
// the fix: a deleted Ingress reaches the removal sink seconds after the
// event instead of waiting out resyncInterval, which is what keeps the
// teardown ahead of the dispatcher's pending window.
func TestWatcher_nudge_tearsDownRemovedIngress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const host = "api.example.test"
	lister := &countingLister{items: []*networkingv1.Ingress{ing("ns", "api", host), ing("ns", "web", "web.example.test")}}
	sink := &collectingSink{}
	w := kube.New(newMemStore(), lister, kube.Options{
		ResyncInterval: time.Hour,
		WatchDebounce:  20 * time.Millisecond,
		Materializer:   slugMaterializer{},
		RemovalSink:    sink,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	waitForLists(t, lister, 1, time.Second)

	// The Ingress is deleted; the informer handler nudges.
	lister.setItems(ing("ns", "web", "web.example.test"))
	w.Nudge()
	waitForLists(t, lister, 2, time.Second)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(sink.removed()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := sink.removed(); len(got) != 1 || got[0] != host {
		t.Errorf("removal sink: got %v, want exactly [%s]", got, host)
	}
	cancel()
	<-done
}

// TestWatcher_nudge_recreateInsideWindowIsNoop pins the debounce's second
// job: it is also the settle window. A delete immediately followed by a
// recreate (a helm apply replacing an Ingress) must not tear the monitor
// down, because the pass re-lists the cache after the window rather than
// acting on the event that woke it.
func TestWatcher_nudge_recreateInsideWindowIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const host = "api.example.test"
	lister := &countingLister{items: []*networkingv1.Ingress{ing("ns", "api", host)}}
	sink := &collectingSink{}
	w := kube.New(newMemStore(), lister, kube.Options{
		ResyncInterval: time.Hour,
		WatchDebounce:  200 * time.Millisecond,
		Materializer:   slugMaterializer{},
		RemovalSink:    sink,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	waitForLists(t, lister, 1, time.Second)

	// Delete, nudge, then recreate before the window elapses.
	lister.setItems()
	w.Nudge()
	lister.setItems(ing("ns", "api", host))

	waitForLists(t, lister, 2, 2*time.Second)
	if got := sink.removed(); len(got) != 0 {
		t.Errorf("a recreate inside the debounce window must not tear anything down, got removals %v", got)
	}
	cancel()
	<-done
}

// TestWatcher_nudge_disabled is the escape hatch: watchDebounce 0 leaves
// the resync ticker as the only trigger, so an operator can revert to the
// previous behaviour by config alone.
func TestWatcher_nudge_disabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lister := &countingLister{items: []*networkingv1.Ingress{ing("ns", "api", "api.example.test")}}
	w := kube.New(newMemStore(), lister, kube.Options{
		ResyncInterval: time.Hour,
		WatchDebounce:  0,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	waitForLists(t, lister, 1, time.Second)

	for range 5 {
		w.Nudge()
	}
	time.Sleep(100 * time.Millisecond)
	if got := lister.listCount(); got != 1 {
		t.Errorf("with watchDebounce disabled: got %d lists, want 1 (nudges must be inert)", got)
	}
	cancel()
	<-done
}

// TestWatcher_nudge_burstCollapsesToOneReconcile is the load guard: a
// rolling deploy touches many Ingresses at once, and each event must not
// buy its own reconcile pass. The whole burst lands inside one debounce
// window, so it costs exactly one pass.
func TestWatcher_nudge_burstCollapsesToOneReconcile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const debounce = 100 * time.Millisecond
	lister := &countingLister{items: []*networkingv1.Ingress{ing("ns", "api", "api.example.test")}}
	w := kube.New(newMemStore(), lister, kube.Options{
		ResyncInterval: time.Hour,
		WatchDebounce:  debounce,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	waitForLists(t, lister, 1, time.Second)

	// 50 events, all well inside one debounce window.
	for range 50 {
		w.Nudge()
	}

	if got := waitForLists(t, lister, 2, time.Second); got != 2 {
		t.Fatalf("burst reconcile: got %d lists, want 2 (startup + one collapsed pass)", got)
	}
	// Nothing queued behind it: the burst bought one pass, not fifty.
	time.Sleep(3 * debounce)
	if got := lister.listCount(); got != 2 {
		t.Errorf("after the debounce window: got %d lists, want 2 — the burst should not have queued further passes", got)
	}
	cancel()
	<-done
}
