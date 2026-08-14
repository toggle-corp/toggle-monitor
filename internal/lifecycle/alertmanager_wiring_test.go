package lifecycle

import (
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
	"github.com/toggle-corp/toggle-monitor/internal/kube"
	"github.com/toggle-corp/toggle-monitor/internal/merger"
)

type fakeKubeConsumer struct {
	src merger.NamespaceAnnotationSource
}

func (f *fakeKubeConsumer) SetNamespaceAnnotationSource(src merger.NamespaceAnnotationSource) {
	f.src = src
}

type fakeAMConsumer struct {
	src alertmanager.NamespaceAnnotationSource
}

func (f *fakeAMConsumer) SetNamespaceAnnotationSource(src alertmanager.NamespaceAnnotationSource) {
	f.src = src
}

// Both cascades read namespace annotations. Wiring only the kube one is
// the silent failure mode: annotation-sourced alertmanager routing
// degrades to the root channel and nothing but a counter moves.
func TestWireNamespaceAnnotations_wiresBothCascades(t *testing.T) {
	watcher := &kube.Watcher{}
	mat := &fakeKubeConsumer{}
	am := &fakeAMConsumer{}

	wireNamespaceAnnotations(watcher, mat, am)

	if mat.src == nil {
		t.Error("kube materializer did not receive the annotation source")
	}
	if am.src == nil {
		t.Error("alertmanager handler did not receive the annotation source")
	}
}

// A deployment may configure either cascade alone.
func TestWireNamespaceAnnotations_skipsAbsentConsumers(t *testing.T) {
	watcher := &kube.Watcher{}

	am := &fakeAMConsumer{}
	wireNamespaceAnnotations(watcher, nil, am)
	if am.src == nil {
		t.Error("alertmanager handler should still be wired with no materializer")
	}

	mat := &fakeKubeConsumer{}
	wireNamespaceAnnotations(watcher, mat, nil)
	if mat.src == nil {
		t.Error("materializer should still be wired with no alertmanager handler")
	}

	// A typed-nil consumer (the shape lifecycle passes when a block is
	// absent) must not panic.
	var nilAM *alertmanager.Handler
	wireNamespaceAnnotations(watcher, mat, nilAM)
}

// No watcher means no informer to read through.
func TestWireNamespaceAnnotations_noSource_isNoop(t *testing.T) {
	mat := &fakeKubeConsumer{}
	am := &fakeAMConsumer{}
	wireNamespaceAnnotations(nil, mat, am)
	if mat.src != nil || am.src != nil {
		t.Error("a nil watcher should wire nothing")
	}
}
