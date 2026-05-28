// Package depindex holds the reverse-dependsOn index: a precomputed
// mapping from a parent monitor's slug to the slugs of every monitor
// that depends on it.
//
// The forward direction (monitor → its dependsOn parents) lives on
// every Plan / Monitor; the reverse direction is what push-propagation
// (ADR-0004) and the on-demand parent-probe pass need: "the parent
// just opened an incident — which children must I pause?". Computing
// it from the forward direction is O(N·D) per query (N monitors, D
// avg deps); building the index once at startup makes lookup O(1) per
// parent and O(C) to iterate its C children.
//
// The index is read-only after construction. Lifecycle owns it: it
// builds it from the resolved Plan list and injects it into the
// scheduler (for push) and the coalesce manager (for redaction inside
// the on-demand probe).
package depindex

import "sort"

// Spec is the minimal input shape: a monitor's slug + its dependsOn
// parents. The package deliberately does not import scheduler.Plan or
// config.Monitor to avoid cycles; callers adapt their own struct into
// this with a trivial conversion.
type Spec struct {
	Slug      string
	DependsOn []string
}

// Index is the reverse-dependsOn lookup. The zero value is not usable;
// always construct via Build.
type Index struct {
	children map[string][]string // parent → sorted, deduplicated children
}

// Build constructs the reverse-dependsOn index from a list of specs.
// Child lists are sorted lexicographically (deterministic logs, stable
// test assertions) and deduplicated (a spec listing the same parent
// twice contributes one entry, not two).
//
// Unknown parent references (a child depends on a slug not present in
// `specs`) are still indexed — Build is purely structural. The config
// layer catches unknown slugs before this layer is consulted.
func Build(specs []Spec) *Index {
	idx := &Index{children: map[string][]string{}}
	// First pass: collect into per-parent sets (slice + linear scan
	// would be cheaper for tiny D, but a set keeps the dedup invariant
	// trivial to read).
	seen := map[string]map[string]struct{}{}
	for _, s := range specs {
		for _, parent := range s.DependsOn {
			set, ok := seen[parent]
			if !ok {
				set = map[string]struct{}{}
				seen[parent] = set
			}
			set[s.Slug] = struct{}{}
		}
	}
	// Second pass: materialize sorted slices.
	for parent, set := range seen {
		out := make([]string, 0, len(set))
		for child := range set {
			out = append(out, child)
		}
		sort.Strings(out)
		idx.children[parent] = out
	}
	return idx
}

// Children returns the sorted slugs of monitors that depend on
// `parent`. Returns nil when no monitor depends on `parent` (including
// the case where `parent` is itself unknown). Callers must not mutate
// the returned slice.
func (i *Index) Children(parent string) []string {
	if i == nil {
		return nil
	}
	return i.children[parent]
}
