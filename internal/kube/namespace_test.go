package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

// ADR-0009 routes namespace-scoped `*From` blocks through a Namespace
// informer. Namespace is the scope an ownership map can be stated at
// once, rather than repeated per ingress.

func TestNamespaceInformerLister_returnsAnnotations(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "acme-service-a-1",
			Annotations: map[string]string{"app.example.test/slack": "ops-alerts"},
		},
	})
	factory := informers.NewSharedInformerFactory(client, 0)
	informer := factory.Core().V1().Namespaces()
	_ = informer.Informer()
	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)
	factory.WaitForCacheSync(stop)

	lister := &namespaceInformerLister{lister: informer.Lister()}

	got := lister.NamespaceAnnotations("acme-service-a-1")
	if got["app.example.test/slack"] != "ops-alerts" {
		t.Errorf("annotations = %v, want the slack annotation", got)
	}
}

// A namespace the informer has never seen must resolve to nothing
// rather than erroring: the *From block then falls back to its
// default, and monitoring keeps running.
func TestNamespaceInformerLister_unknownNamespaceIsEmpty(t *testing.T) {
	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	informer := factory.Core().V1().Namespaces()
	_ = informer.Informer()
	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)
	factory.WaitForCacheSync(stop)

	lister := &namespaceInformerLister{lister: informer.Lister()}

	if got := lister.NamespaceAnnotations("nope"); len(got) != 0 {
		t.Errorf("annotations = %v, want empty", got)
	}
}
