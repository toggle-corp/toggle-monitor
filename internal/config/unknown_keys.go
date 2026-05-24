package config

import (
	"reflect"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// validateUnknownKeys walks the YAML node tree alongside Config's
// reflected struct shape and reports any mapping key that doesn't
// appear in the corresponding struct's yaml tags. Maps (map[K]V) are
// user-keyed and skipped. YAML merge keys (`<<:`) are followed and
// the merged-in keys are checked against the destination struct's
// allowlist. The `x-*` escape hatch is honoured only at the top-level
// Config mapping (anchor-only hosts, docker-compose convention).
func (c *checker) validateUnknownKeys() {
	if c.root == nil {
		return
	}
	top := c.root
	if top.Kind == yaml.DocumentNode && len(top.Content) > 0 {
		top = top.Content[0]
	}
	c.walkUnknownKeys(top, reflect.TypeOf(Config{}), nil)
}

// allowedKeysCache memoises the yaml-tag set for each struct type.
var allowedKeysCache sync.Map // reflect.Type → []string

// allowedKeysFor returns the sorted set of yaml tag names declared on
// the fields of t. Fields tagged `yaml:"-"` or with no tag are ignored.
func allowedKeysFor(t reflect.Type) []string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	if cached, ok := allowedKeysCache.Load(t); ok {
		return cached.([]string)
	}
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		name := yamlFieldName(t.Field(i))
		if name == "" {
			continue
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	allowedKeysCache.Store(t, keys)
	return keys
}

// fieldTypeForKey returns the Go type of the struct field whose yaml
// tag matches key, or nil if no field matches.
func fieldTypeForKey(t reflect.Type, key string) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		if yamlFieldName(t.Field(i)) == key {
			return t.Field(i).Type
		}
	}
	return nil
}

// yamlFieldName extracts the yaml tag name for a struct field. Returns
// "" for fields tagged "-" or with no yaml tag — those don't
// contribute to the allowlist.
func yamlFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" || tag == "-" {
		return ""
	}
	if idx := strings.Index(tag, ","); idx >= 0 {
		tag = tag[:idx]
	}
	return tag
}

// walkUnknownKeys recurses into node, dispatching on the Go type it
// expects to find there. Scalar leaves terminate the walk; unknown Go
// kinds are ignored (the schema only uses struct / slice / map /
// pointer / scalar shapes).
func (c *checker) walkUnknownKeys(node *yaml.Node, t reflect.Type, path []any) {
	if node == nil || t == nil {
		return
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		c.walkUnknownKeys(node.Alias, t, path)
		return
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return
		}
		c.walkUnknownKeysMapping(node, t, path)
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return
		}
		elemT := t.Elem()
		for i, child := range node.Content {
			c.walkUnknownKeys(child, elemT, append(append([]any{}, path...), i))
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return
		}
		valT := t.Elem()
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]
			keyStr := ""
			if keyNode.Kind == yaml.ScalarNode {
				keyStr = keyNode.Value
			}
			c.walkUnknownKeys(valNode, valT, append(append([]any{}, path...), keyStr))
		}
	}
}

// walkUnknownKeysMapping is the struct + MappingNode case. It checks
// every present key against the struct's allowlist, follows `<<:`
// merge keys, and recurses into matched fields.
func (c *checker) walkUnknownKeysMapping(node *yaml.Node, t reflect.Type, path []any) {
	isTopLevel := t == reflect.TypeOf(Config{})
	allowed := allowedKeysFor(t)
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		allowedSet[k] = struct{}{}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		if keyNode.Kind != yaml.ScalarNode {
			continue
		}
		key := keyNode.Value
		if key == "<<" {
			c.walkMergedMapping(valNode, t, path)
			continue
		}
		if isTopLevel && strings.HasPrefix(key, "x-") {
			continue
		}
		if _, ok := allowedSet[key]; !ok {
			c.errfNode(keyNode, path,
				"unknown key %q; allowed keys here: [%s]", key, strings.Join(allowed, ", "))
			continue
		}
		if fieldT := fieldTypeForKey(t, key); fieldT != nil {
			c.walkUnknownKeys(valNode, fieldT, append(append([]any{}, path...), key))
		}
	}
}

// walkMergedMapping follows a YAML merge-key value — an alias, an
// inline mapping, or a sequence of those — and validates each present
// key against the destination struct type t. Keys merged in via
// anchors must satisfy the same allowlist as keys spelled inline.
func (c *checker) walkMergedMapping(node *yaml.Node, t reflect.Type, path []any) {
	if node == nil {
		return
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		c.walkMergedMapping(node.Alias, t, path)
		return
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			c.walkMergedMapping(child, t, path)
		}
		return
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	c.walkUnknownKeysMapping(node, t, path)
}
