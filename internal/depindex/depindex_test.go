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

func TestDescendants_nestedTreeBreadthFirst(t *testing.T) {
	idx := depindex.Build([]depindex.Spec{
		{Slug: "root"},
		{Slug: "mid", DependsOn: []string{"root"}},
		{Slug: "api", DependsOn: []string{"root"}},
		{Slug: "leaf-b", DependsOn: []string{"mid"}},
		{Slug: "leaf-a", DependsOn: []string{"mid"}},
		{Slug: "deep", DependsOn: []string{"leaf-a"}},
	})
	want := []string{"api", "mid", "leaf-a", "leaf-b", "deep"}
	if got := idx.Descendants("root"); !reflect.DeepEqual(got, want) {
		t.Errorf("Descendants(root): got %v, want %v", got, want)
	}
	if got := idx.Descendants("deep"); got != nil {
		t.Errorf("Descendants(deep): got %v, want nil", got)
	}
}

func TestDescendants_diamondListedOnce(t *testing.T) {
	idx := depindex.Build([]depindex.Spec{
		{Slug: "root"},
		{Slug: "left", DependsOn: []string{"root"}},
		{Slug: "right", DependsOn: []string{"root"}},
		{Slug: "bottom", DependsOn: []string{"left", "right"}},
	})
	want := []string{"left", "right", "bottom"}
	if got := idx.Descendants("root"); !reflect.DeepEqual(got, want) {
		t.Errorf("Descendants(root): got %v, want %v", got, want)
	}
}

func TestDescendants_cycleTerminates(t *testing.T) {
	// Config validation rejects cycles; the walk still terminates and
	// never lists the starting monitor.
	idx := depindex.Build([]depindex.Spec{
		{Slug: "a", DependsOn: []string{"b"}},
		{Slug: "b", DependsOn: []string{"a"}},
	})
	if got := idx.Descendants("a"); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("Descendants(a): got %v, want [b]", got)
	}
}
