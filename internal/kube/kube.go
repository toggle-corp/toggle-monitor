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
//
// PruneDiscoverySnapshot now returns the slugs of materialized
// monitors that disappeared so the watcher can hand them off to a
// RemovalSink for soft-delete + Slack closeout.
type SnapshotStore interface {
	UpsertDiscoverySnapshot(ctx context.Context, row store.DiscoverySnapshotRow) error
	PruneDiscoverySnapshot(ctx context.Context, before time.Time) (int64, []string, error)
}

// RemovalSink is the seam the watcher calls when a kube-discovered
// monitor's ingress disappears from the cluster. Production wires
// lifecycle.kubeRemovalSink (which soft-deletes the monitor + posts
// the closeout + warning via the Slack notifier).
type RemovalSink interface {
	OnKubeMonitorRemoved(ctx context.Context, monitorSlug string)
}

// IngressLister abstracts the informer's lister so tests can provide a
// hand-built slice without a fake informer setup.
type IngressLister interface {
	List() ([]*networkingv1.Ingress, error)
}

// Watcher owns the informer lifecycle and the periodic reconcile pass.
type Watcher struct {
	store          SnapshotStore
	lister         IngressLister
	resyncInterval time.Duration
	log            *slog.Logger
	now            func() time.Time

	// Optional materializer: when set, the watcher delegates per-host
	// decisions (added / kube-ignored / kube-invalid sub-reason) to
	// this seam. When nil — e.g. the kube block is missing or the
	// merger couldn't be constructed — every observed host is
	// recorded as kube-invalid with a generic placeholder reason.
	materialize Materializer

	// Optional removal sink: invoked once per materialized monitor
	// whose snapshot row gets pruned. Nil disables the callback.
	onRemoval RemovalSink
}

// Materializer is the seam Issue-9 plugs in to do the real
// preset+annotation merge. It examines one (ingress, host) pair and
// returns the snapshot row that should be persisted (and side-effects
// like reconciling the materialized monitor are the materializer's
// responsibility).
type Materializer interface {
	Materialize(ctx context.Context, ing *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error)
}

// Pruner is an optional interface a Materializer can implement to
// receive a post-reconcile "drop everything you didn't see this
// pass" signal. Mirrors the snapshot-table pruning the watcher does
// directly.
type Pruner interface {
	Prune(before time.Time)
}

// Options configures a Watcher.
type Options struct {
	ResyncInterval time.Duration
	Materializer   Materializer
	RemovalSink    RemovalSink
	Logger         *slog.Logger
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
		store:          s,
		lister:         lister,
		resyncInterval: opts.ResyncInterval,
		log:            opts.Logger,
		now:            time.Now,
		materialize:    opts.Materializer,
		onRemoval:      opts.RemovalSink,
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
	// Heartbeat for the operator: confirms the watcher is alive and
	// reaching the cluster. Zero is a normal observation (fresh
	// cluster, RBAC scoped to a namespace with no ingresses, etc.)
	// but the empty-state UI can't tell that apart from "watcher
	// never ran" without this line in the log.
	if len(ingresses) == 0 {
		w.log.Info("kube reconcile: no ingresses observed in cluster")
	} else {
		w.log.Info("kube reconcile", "ingresses", len(ingresses))
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
	// Sweep rows we didn't observe this pass. Each removed snapshot
	// row that pointed at a materialized monitor flows through the
	// optional RemovalSink so the lifecycle can soft-delete + post
	// the closeout + warning.
	_, prunedMonitors, err := w.store.PruneDiscoverySnapshot(ctx, startedAt)
	if err != nil {
		w.log.Warn("prune discovery snapshot", "error", err)
	}
	if w.onRemoval != nil {
		for _, slug := range prunedMonitors {
			w.onRemoval.OnKubeMonitorRemoved(ctx, slug)
		}
	}
	if p, ok := w.materialize.(Pruner); ok {
		p.Prune(startedAt)
	}
	return nil
}

// snapshotRowFor builds the snapshot row for a single (ingress, host)
// pair. When the watcher has a materializer it delegates; otherwise it
// records every ingress as kube-invalid with a generic placeholder
// reason (this branch should not run in production — the materializer
// is wired whenever cfg.Kube is non-nil — but keeping it ensures
// observe-only deployments still leave a trace per discovered host).
func (w *Watcher) snapshotRowFor(ctx context.Context, ing *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error) {
	if w.materialize != nil {
		row, err := w.materialize.Materialize(ctx, ing, host)
		if err != nil {
			return store.DiscoverySnapshotRow{}, err
		}
		return row, nil
	}
	reason := "no materializer configured"
	return store.DiscoverySnapshotRow{
		Namespace:   ing.Namespace,
		IngressName: ing.Name,
		Host:        host,
		Status:      "kube-invalid",
		Reason:      &reason,
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

// --- production client-go setup -----------------------------------

// NewWithCluster boots client-go (in-cluster ServiceAccount or
// kubeconfig fallback), starts an Ingress informer, and returns a
// Watcher whose lister is backed by the informer's cache. The caller
// is responsible for calling Watcher.Run(ctx) to drive the reconcile
// loop.
func NewWithCluster(ctx context.Context, s SnapshotStore, opts Options, kubeconfigPath string) (*Watcher, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	cfg, err := loadClusterConfig(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load kube client config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}

	// Connectivity probe — surface auth/network errors immediately
	// with a clear message instead of letting them turn into a silent
	// empty cache later. Discovery uses the same TLS + auth path as
	// the informer; if this fails, the informer would have too.
	ver, err := cs.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("kube api unreachable (server=%s): %w", cfg.Host, err)
	}
	log.Info("kube api reachable", "server", cfg.Host, "version", ver.GitVersion)

	factory := informers.NewSharedInformerFactory(cs, opts.ResyncInterval)
	// IMPORTANT: register the informer with the factory BEFORE calling
	// Start. Calling Networking().V1().Ingresses() alone returns the
	// typed wrapper without registering — Informer() forces the
	// SharedIndexInformer into the factory's map so Start actually
	// runs it. Without this, Start runs nothing, the cache stays
	// empty forever, and List() silently returns []. (#kube-empty-bug)
	ingInformer := factory.Networking().V1().Ingresses()
	_ = ingInformer.Informer()

	factory.Start(ctx.Done())
	synced := factory.WaitForCacheSync(ctx.Done())
	for typ, ok := range synced {
		if !ok {
			return nil, fmt.Errorf("kube informer cache failed to sync for %v (RBAC denied or API unreachable?)", typ)
		}
	}

	w := New(s, &ingressInformerLister{lister: ingInformer.Lister()}, opts)
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
