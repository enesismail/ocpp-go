package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func fixturePath(parts ...string) string {
	path := "testdata"
	for _, part := range parts {
		path = filepath.Join(path, part)
	}
	return path
}

func requireImplemented(t *testing.T, operation string, err error) {
	t.Helper()
	if errors.Is(err, errNotImplemented) {
		t.Fatalf("%s is not implemented: %+v", operation, err)
	}
	if err != nil {
		t.Fatalf("%s failed before its behavior could be checked: %+v", operation, err)
	}
}

func requireExpectedHardFailure(t *testing.T, operation, want string, err error) {
	t.Helper()
	requireExpectedHardFailureContains(t, operation, err, want)
}

// requireExpectedHardFailureContains asserts err is a real hard failure (not
// errNotImplemented, not nil) whose message contains every string in wants —
// use it when a single filename substring is not enough to tell a correct
// check apart from a field-blind one that merely echoes its input path.
func requireExpectedHardFailureContains(t *testing.T, operation string, err error, wants ...string) {
	t.Helper()
	if errors.Is(err, errNotImplemented) {
		t.Fatalf("%s is not implemented; expected a hard failure containing %q: %+v", operation, wants, err)
	}
	if err == nil {
		t.Fatalf("%s accepted invalid input; expected a hard failure containing %q", operation, wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s hard failure %+v does not name %q", operation, err, want)
		}
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func findDefinition(definitions []Definition, name string) *Definition {
	for index := range definitions {
		if definitions[index].Name == name {
			return &definitions[index]
		}
	}
	return nil
}

func findProperty(properties []Property, name string) *Property {
	for index := range properties {
		if properties[index].Name == name {
			return &properties[index]
		}
	}
	return nil
}
