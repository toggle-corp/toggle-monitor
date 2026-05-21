// Package kube watches Kubernetes Ingress resources cluster-wide and
// maintains the auto-discovery snapshot.
package kube

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// SnapshotStore is the slim seam the watcher uses to persist
// discovery rows. Production wires *store.Repo; tests inject a fake.
type SnapshotStore interface {
	UpsertDiscoverySnapshot(ctx context.Context, row store.DiscoverySnapshotRow) error
	PruneDiscoverySnapshot(ctx context.Context, before time.Time) (int64, error)
}

// IngressLister abstracts the informer's lister so tests can provide a
// hand-built slice without a fake informer setup.
type IngressLister interface {
	List() ([]*networkingv1.Ingress, error)
}

// Watcher owns the informer lifecycle and the periodic reconcile pass.
type Watcher struct {
	store            SnapshotStore
	lister           IngressLister
	annotationDomain string
	resyncInterval   time.Duration
	log              *slog.Logger
	now              func() time.Time

	// Optional materializer: when set, the watcher delegates per-host
	// decisions (added / kube-paused / kube-invalid sub-reason) to
	// this seam. When nil (Issue 8 observe-only), every observed
	// host is recorded as kube-invalid with reason
	// "no preset annotation".
	materialize Materializer
}

// Materializer is the seam Issue-9 plugs in to do the real
// preset+annotation merge. It examines one (ingress, host) pair and
// returns the snapshot row that should be persisted (and side-effects
// like reconciling the materialized monitor are the materializer's
// responsibility).
type Materializer interface {
	Materialize(ctx context.Context, ing *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error)
}

// Options configures a Watcher.
type Options struct {
	AnnotationDomain string
	ResyncInterval   time.Duration
	Materializer     Materializer
	Logger           *slog.Logger
}

// New constructs a Watcher against a SnapshotStore + IngressLister.
// Most production code paths use NewWithCluster which builds the
// lister from a kubeconfig-or-in-cluster client.
func New(s SnapshotStore, lister IngressLister, opts Options) *Watcher {
	if opts.ResyncInterval <= 0 {
		opts.ResyncInterval = 30 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Watcher{
		store:            s,
		lister:           lister,
		annotationDomain: opts.AnnotationDomain,
		resyncInterval:   opts.ResyncInterval,
		log:              opts.Logger,
		now:              time.Now,
		materialize:      opts.Materializer,
	}
}

// Run performs an initial reconcile and then re-reconciles every
// resyncInterval until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	if err := w.Reconcile(ctx); err != nil {
		w.log.Error("kube reconcile failed", "error", err)
	}
	t := time.NewTicker(w.resyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Reconcile(ctx); err != nil {
				w.log.Error("kube reconcile failed", "error", err)
			}
		}
	}
}

// Reconcile walks every observed Ingress + host, upserts the
// resulting snapshot row, then prunes rows that weren't touched.
// Exported so tests can drive a single pass deterministically.
func (w *Watcher) Reconcile(ctx context.Context) error {
	startedAt := w.now()
	ingresses, err := w.lister.List()
	if err != nil {
		return fmt.Errorf("list ingresses: %w", err)
	}
	for _, ing := range ingresses {
		for _, host := range uniqueHosts(ing) {
			row, err := w.snapshotRowFor(ctx, ing, host)
			if err != nil {
				w.log.Warn("materialize ingress host", "ns", ing.Namespace, "name", ing.Name, "host", host, "error", err)
				continue
			}
			if err := w.store.UpsertDiscoverySnapshot(ctx, row); err != nil {
				w.log.Warn("upsert discovery snapshot", "ns", ing.Namespace, "name", ing.Name, "host", host, "error", err)
			}
		}
	}
	// Sweep rows we didn't observe this pass.
	if _, err := w.store.PruneDiscoverySnapshot(ctx, startedAt); err != nil {
		w.log.Warn("prune discovery snapshot", "error", err)
	}
	return nil
}

// snapshotRowFor builds the snapshot row for a single (ingress, host)
// pair. When the watcher has a materializer (Issue 9+) it delegates;
// otherwise it records every ingress as kube-invalid with reason
// "no preset annotation" — the Issue-8 observe-only behavior.
func (w *Watcher) snapshotRowFor(ctx context.Context, ing *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error) {
	if w.materialize != nil {
		row, err := w.materialize.Materialize(ctx, ing, host)
		if err != nil {
			return store.DiscoverySnapshotRow{}, err
		}
		return row, nil
	}
	reason := "no preset annotation"
	return store.DiscoverySnapshotRow{
		Namespace:   ing.Namespace,
		IngressName: ing.Name,
		Host:        host,
		Status:      "kube-invalid",
		Reason:      &reason,
		Annotations: copyAnnotations(ing.Annotations),
	}, nil
}

// uniqueHosts returns the deduped, non-empty set of `host` fields
// from spec.rules[].host. Order is preserved (first observed wins).
func uniqueHosts(ing *networkingv1.Ingress) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			continue
		}
		if _, dup := seen[rule.Host]; dup {
			continue
		}
		seen[rule.Host] = struct{}{}
		out = append(out, rule.Host)
	}
	return out
}

func copyAnnotations(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// AnnotationDomain returns the configured base annotation domain
// (e.g. "monitor.togglecorp.com"). Exposed for the materializer.
func (w *Watcher) AnnotationDomain() string { return w.annotationDomain }

// --- production client-go setup -----------------------------------

// NewWithCluster boots client-go (in-cluster ServiceAccount or
// kubeconfig fallback), starts an Ingress informer, and returns a
// Watcher whose lister is backed by the informer's cache. The caller
// is responsible for calling Watcher.Run(ctx) to drive the reconcile
// loop.
func NewWithCluster(ctx context.Context, s SnapshotStore, opts Options, kubeconfigPath string) (*Watcher, error) {
	cfg, err := loadClusterConfig(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load kube client config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	factory := informers.NewSharedInformerFactory(cs, opts.ResyncInterval)
	inf := factory.Networking().V1().Ingresses()
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	w := New(s, &ingressInformerLister{lister: inf.Lister()}, opts)
	return w, nil
}

// ingressInformerLister wraps the client-go informer's typed lister
// behind our IngressLister interface.
type ingressInformerLister struct {
	lister interface {
		List(selector labels.Selector) ([]*networkingv1.Ingress, error)
	}
}

func (l *ingressInformerLister) List() ([]*networkingv1.Ingress, error) {
	return l.lister.List(labels.Everything())
}

func loadClusterConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	return rest.InClusterConfig()
}
