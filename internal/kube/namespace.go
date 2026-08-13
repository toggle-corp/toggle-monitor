package kube

import (
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// NamespaceAnnotationLister reads a namespace's annotations from the
// informer cache. It is the seam ADR-0009's namespaceAnnotation-scoped
// `*From` blocks resolve through, and the reason toggle-monitor needs
// `get/list/watch namespaces` beyond its Ingress watch.
//
// A namespace the cache has never seen resolves to an empty map rather
// than an error: those sources then fall back to their defaults, which
// keeps a namespace-read hiccup from costing availability monitoring.
type NamespaceAnnotationLister interface {
	NamespaceAnnotations(namespace string) map[string]string
}

// namespaceInformerLister adapts client-go's typed Namespace lister to
// NamespaceAnnotationLister.
type namespaceInformerLister struct {
	lister corev1listers.NamespaceLister
}

func (l *namespaceInformerLister) NamespaceAnnotations(namespace string) map[string]string {
	ns, err := l.lister.Get(namespace)
	if err != nil || ns == nil {
		return nil
	}
	return ns.Annotations
}

// NamespaceAnnotations implements NamespaceAnnotationLister on the
// Watcher, so callers hold one object rather than two. Returns nil when
// the watcher was built without a namespace informer (the injected-
// lister test path).
func (w *Watcher) NamespaceAnnotations(namespace string) map[string]string {
	if w.namespaces == nil {
		return nil
	}
	return w.namespaces.NamespaceAnnotations(namespace)
}
