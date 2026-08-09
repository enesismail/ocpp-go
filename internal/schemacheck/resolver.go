package main

import (
	"fmt"
	"strings"
)

// Resolve resolves selector (a "pkg.Type" reference as written in file) to a
// fully-qualified type identity, using only the import bindings registered
// for that specific file — the same selector text is expected to resolve
// differently in different files, because it is each file's own import
// bindings, never a shared table, that give the selector its meaning.
func (r *TypeResolver) Resolve(file, selector string) (string, error) {
	prefix, name, ok := strings.Cut(selector, ".")
	if !ok || prefix == "" || name == "" {
		return "", fmt.Errorf("unknown selector %q in %s: not a package-qualified selector", selector, file)
	}
	for _, binding := range r.imports[file] {
		if binding.Alias == prefix {
			return binding.Path + "." + name, nil
		}
	}
	return "", fmt.Errorf("unknown selector %q in %s: no import binding registered for alias %q", selector, file, prefix)
}
