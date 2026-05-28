package depindex_test

import (
	"reflect"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/depindex"
)

func TestBuild_emptyInput(t *testing.T) {
	idx := depindex.Build(nil)
	if got := idx.Children("anything"); got != nil {
		t.Errorf("Children on empty index: got %v, want nil", got)
	}
}

func TestBuild_singleDependency(t *testing.T) {
	idx := depindex.Build([]depindex.Spec{
		{Slug: "bastion"},
		{Slug: "api", DependsOn: []string{"bastion"}},
	})
	if got := idx.Children("bastion"); !reflect.DeepEqual(got, []string{"api"}) {
		t.Errorf("Children(bastion): got %v, want [api]", got)
	}
	if got := idx.Children("api"); got != nil {
		t.Errorf("Children(api): got %v, want nil (api has no dependents)", got)
	}
}

func TestBuild_multipleChildren(t *testing.T) {
	idx := depindex.Build([]depindex.Spec{
		{Slug: "bastion"},
		// Intentional unsorted input — Build must return sorted child lists.
		{Slug: "worker", DependsOn: []string{"bastion"}},
		{Slug: "api", DependsOn: []string{"bastion"}},
		{Slug: "scheduler", DependsOn: []string{"bastion"}},
	})
	got := idx.Children("bastion")
	want := []string{"api", "scheduler", "worker"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Children(bastion): got %v, want %v (sorted)", got, want)
	}
}

func TestBuild_multiParentChild(t *testing.T) {
	// One child appears under each parent's children list.
	idx := depindex.Build([]depindex.Spec{
		{Slug: "bastion"},
		{Slug: "db"},
		{Slug: "api", DependsOn: []string{"bastion", "db"}},
	})
	if got := idx.Children("bastion"); !reflect.DeepEqual(got, []string{"api"}) {
		t.Errorf("Children(bastion): got %v, want [api]", got)
	}
	if got := idx.Children("db"); !reflect.DeepEqual(got, []string{"api"}) {
		t.Errorf("Children(db): got %v, want [api]", got)
	}
}

func TestBuild_dedupesDuplicateDeps(t *testing.T) {
	// Defensive: a misconfigured spec listing the same parent twice
	// must not produce a duplicated child entry.
	idx := depindex.Build([]depindex.Spec{
		{Slug: "bastion"},
		{Slug: "api", DependsOn: []string{"bastion", "bastion"}},
	})
	if got := idx.Children("bastion"); !reflect.DeepEqual(got, []string{"api"}) {
		t.Errorf("Children(bastion): got %v, want [api] (deduped)", got)
	}
}

func TestBuild_unknownParentReferencesAreIndexed(t *testing.T) {
	// Build is purely structural — it does not validate that the
	// referenced parent slug actually exists in the spec list. The
	// config validator catches unknown slugs before this layer.
	idx := depindex.Build([]depindex.Spec{
		{Slug: "api", DependsOn: []string{"phantom-parent"}},
	})
	if got := idx.Children("phantom-parent"); !reflect.DeepEqual(got, []string{"api"}) {
		t.Errorf("unknown parent should still be indexable; got %v", got)
	}
}
