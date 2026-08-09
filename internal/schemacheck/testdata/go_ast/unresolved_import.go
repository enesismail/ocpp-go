package fixture

import unknownpkg "example.com/unknownpkg"

// PayloadWithUnresolvedImport exercises the walker-level hard failure: the
// field below uses a package-qualified selector, but nothing in this test
// registers this file's own import binding with the resolver before the
// walk. A conformant walker cannot guess what the selector means and must
// hard-fail naming the file, line and selector — never silently skip the
// field or fall back to treating it as a struct to recurse into.
type PayloadWithUnresolvedImport struct {
	Value unknownpkg.Thing `json:"value"`
}
