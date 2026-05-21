// Package kube watches Kubernetes Ingress resources cluster-wide and
// maintains the auto-discovery snapshot.
package kube

import (
	// client-go pieces used by the informer; locked here in advance of
	// the watcher landing in Issue 8.
	_ "k8s.io/api/networking/v1"
	_ "k8s.io/client-go/informers"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/rest"
	_ "k8s.io/client-go/tools/clientcmd"
)
