// Package kube watches Kubernetes Ingress resources cluster-wide and
// maintains the auto-discovery snapshot.
package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	networkingv1listers "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	tmsentry "github.com/toggle-corp/toggle-monitor/internal/sentry"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// SnapshotStore is the slim seam the watcher uses to persist
// discovery rows. Production wires *store.Repo; tests inject a fake.
//
// PruneDiscoverySnapshotExcept returns the slugs of materialized
// monitors that disappeared so the watcher can hand them off to a
// RemovalSink for soft-delete + Slack closeout. The observed-set
// argument is the (ns, name, host) tuples the watcher actually
// upserted this pass; anything else in the table is treated as
// "no longer in the cluster" and pruned.
type SnapshotStore interface {
	UpsertDiscoverySnapshot(ctx context.Context, row store.DiscoverySnapshotRow) error
	PruneDiscoverySnapshotExcept(ctx context.Context, observed []store.DiscoverySnapshotKey) (int64, []string, error)
}

// RemovalSink is the seam the watcher calls when a kube-discovered
// monitor's ingress disappears from the cluster. Production wires
// lifecycle.kubeRemovalSink (which soft-deletes the monitor + posts
// the closeout + warning via the Slack notifier).
type RemovalSink interface {
	OnKubeMonitorRemoved(ctx context.Context, monitorSlug string)
}

// IngressLister abstracts the informer's lister so tests can provide a
// hand-built slice without a fake informer setup. Get returns
// ErrIngressNotFound when no Ingress matches (ns, name); production
// code surfaces this as a "live recompute unavailable" branch in the
// UI rather than a server error.
type IngressLister interface {
	List() ([]*networkingv1.Ingress, error)
	Get(namespace, name string) (*networkingv1.Ingress, error)
}

// ErrIngressNotFound is returned by IngressLister.Get when the
// requested Ingress isn't in the informer cache (deleted, not yet
// observed, or RBAC-filtered out). Callers compare with errors.Is.
var ErrIngressNotFound = errors.New("ingress not found")

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
// directly. The `observed` set is the monitor slugs the materializer
// returned this pass; anything not in it is dropped.
type Pruner interface {
	Prune(observed map[string]struct{})
}

// Options configures a Watcher.
type Options struct {
	ResyncInterval time.Duration
	Materializer   Materializer
	RemovalSink    RemovalSink
	Logger         *slog.Logger
	// Now is a test seam for the watcher's wall-clock reads. Production
	// leaves it nil and the watcher uses time.Now. Tests that need to
	// assert clock-independent behaviour (e.g. the observed-set prune
	// surviving extreme Go-vs-Postgres skew) inject a fake here.
	Now func() time.Time
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
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Watcher{
		store:          s,
		lister:         lister,
		resyncInterval: opts.ResyncInterval,
		log:            opts.Logger,
		now:            now,
		materialize:    opts.Materializer,
		onRemoval:      opts.RemovalSink,
	}
}

// Lister returns the IngressLister the watcher reads from. Used by
// lifecycle to plumb the same informer cache into the web layer's
// cascade source, so the discovery detail page can re-run the
// cascade against fresh data without owning its own informer.
func (w *Watcher) Lister() IngressLister { return w.lister }

// Run performs an initial reconcile and then re-reconciles every
// resyncInterval until ctx is cancelled.
//
// Each reconcile runs inside a panic-recovery wrapper: a panic in
// one pass must not kill the informer goroutine. WARN (not ERROR) on
// reconcile failure — transient k8s API errors are expected and the
// next tick retries; flooding Sentry on apiserver hiccups would
// drown the actually-actionable events.
func (w *Watcher) Run(ctx context.Context) {
	reconcile := func() {
		defer tmsentry.RecoverPanic(w.log, "kube.reconcile")
		if err := w.Reconcile(ctx); err != nil {
			w.log.Warn("kube reconcile failed", "error", err)
		}
	}
	reconcile()
	t := time.NewTicker(w.resyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile()
		}
	}
}

// Reconcile walks every observed Ingress + host, upserts the
// resulting snapshot row, then prunes rows that weren't touched.
// Exported so tests can drive a single pass deterministically.
//
// The prune step is observed-set based — it deletes any snapshot row
// whose (ns, name, host) tuple was not seen in this pass — rather
// than comparing a Go-time watermark against Postgres' last_seen_at.
// An earlier implementation mixed clocks that way and prune-deleted
// rows whose just-completed upsert happened to record last_seen_at
// slightly behind startedAt under normal NTP drift between the
// toggle-monitor pod and the Postgres pod.
func (w *Watcher) Reconcile(ctx context.Context) error {
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

	observedKeys := make([]store.DiscoverySnapshotKey, 0)
	observedSlugs := make(map[string]struct{})
	for _, ing := range ingresses {
		for _, host := range uniqueHosts(ing) {
			// Record the cluster's reality BEFORE we attempt to
			// materialize/upsert: the ingress×host exists in this
			// pass regardless of whether we manage to refresh its
			// row in this iteration. Transient materialize or upsert
			// failures must not cause the prune to soft-delete the
			// monitor on the next step.
			observedKeys = append(observedKeys, store.DiscoverySnapshotKey{
				Namespace:   ing.Namespace,
				IngressName: ing.Name,
				Host:        host,
			})
			row, err := w.snapshotRowFor(ctx, ing, host)
			if err != nil {
				w.log.Warn("materialize ingress host", "ns", ing.Namespace, "name", ing.Name, "host", host, "error", err)
				continue
			}
			if err := w.store.UpsertDiscoverySnapshot(ctx, row); err != nil {
				w.log.Warn("upsert discovery snapshot", "ns", ing.Namespace, "name", ing.Name, "host", host, "error", err)
				continue
			}
			if row.MonitorSlug != nil && *row.MonitorSlug != "" {
				observedSlugs[*row.MonitorSlug] = struct{}{}
			}
		}
	}

	// Safety net: if the lister returned no ingresses, decline to
	// prune. A genuinely empty cluster is rare; a transient empty
	// list (informer cache mid-relist, RBAC blip, etc.) is the
	// scenario that historically nuked every materialized monitor
	// in one pass. Leave stale rows in place — the next reconcile
	// will pick them up.
	if len(ingresses) == 0 {
		return nil
	}

	// Sweep rows we didn't observe this pass. Each removed snapshot
	// row that pointed at a materialized monitor flows through the
	// optional RemovalSink so the lifecycle can soft-delete + post
	// the closeout + warning.
	_, prunedMonitors, err := w.store.PruneDiscoverySnapshotExcept(ctx, observedKeys)
	if err != nil {
		w.log.Warn("prune discovery snapshot", "error", err)
	}
	if w.onRemoval != nil {
		for _, slug := range prunedMonitors {
			w.onRemoval.OnKubeMonitorRemoved(ctx, slug)
		}
	}
	if p, ok := w.materialize.(Pruner); ok {
		p.Prune(observedSlugs)
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
	lister networkingv1listers.IngressLister
}

func (l *ingressInformerLister) List() ([]*networkingv1.Ingress, error) {
	return l.lister.List(labels.Everything())
}

// Get fetches one Ingress from the informer cache. The wrapper maps
// the typed lister's NotFound to our package-level sentinel so the
// UI can render "ingress no longer in cluster" without importing
// client-go.
func (l *ingressInformerLister) Get(namespace, name string) (*networkingv1.Ingress, error) {
	ing, err := l.lister.Ingresses(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrIngressNotFound
		}
		return nil, err
	}
	return ing, nil
}

func loadClusterConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	return rest.InClusterConfig()
}
