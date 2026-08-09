package main

import "testing"

// TestIndexSchemasByFeatureHandlesBareAndSuffixedConventions covers the gap
// that made a real 1.6 run report every one of its 39 messages unpaired: the
// orchestration used to look a message's schema files up by literally
// concatenating "Request.json"/"Response.json" onto the feature name, which
// is the 2.0.1 convention only — 1.6's own request files are named without
// a "Request" suffix at all ("Foo.json"). indexSchemasByFeature reads each
// document's own detected filename shape instead, so a single corpus mixing
// both conventions (as a run spanning both trees would) resolves both
// correctly, and two files that claim the same feature/side is a hard error
// rather than a silent overwrite.
func TestIndexSchemasByFeatureHandlesBareAndSuffixedConventions(t *testing.T) {
	byName := map[string]SchemaDocument{
		"BootNotificationRequest.json":  {File: "BootNotificationRequest.json", Shape: SchemaShape{FilenameShape: "request-suffixed"}},
		"BootNotificationResponse.json": {File: "BootNotificationResponse.json", Shape: SchemaShape{FilenameShape: "response"}},
		"Heartbeat.json":                {File: "Heartbeat.json", Shape: SchemaShape{FilenameShape: "request-bare"}},
		"HeartbeatResponse.json":        {File: "HeartbeatResponse.json", Shape: SchemaShape{FilenameShape: "response"}},
	}

	requests, responses, err := indexSchemasByFeature(byName)
	if err != nil {
		t.Fatalf("indexSchemasByFeature: %v", err)
	}

	if doc, ok := requests["BootNotification"]; !ok || doc.File != "BootNotificationRequest.json" {
		t.Fatalf("requests[BootNotification] = %#v (ok=%v), want BootNotificationRequest.json", doc, ok)
	}
	if doc, ok := requests["Heartbeat"]; !ok || doc.File != "Heartbeat.json" {
		t.Fatalf("requests[Heartbeat] = %#v (ok=%v), want Heartbeat.json (bare 1.6 convention, no Request suffix)", doc, ok)
	}
	if doc, ok := responses["BootNotification"]; !ok || doc.File != "BootNotificationResponse.json" {
		t.Fatalf("responses[BootNotification] = %#v (ok=%v), want BootNotificationResponse.json", doc, ok)
	}
	if doc, ok := responses["Heartbeat"]; !ok || doc.File != "HeartbeatResponse.json" {
		t.Fatalf("responses[Heartbeat] = %#v (ok=%v), want HeartbeatResponse.json", doc, ok)
	}
	if len(requests) != 2 || len(responses) != 2 {
		t.Fatalf("got %d requests / %d responses, want exactly 2 of each: %v / %v", len(requests), len(responses), requests, responses)
	}
}

// TestIndexSchemasByFeatureRejectsDuplicateClaim covers the hard-failure
// path: two files whose own filename shape both resolve to the same
// (featureName, side) pair must stop the run rather than let the second
// silently overwrite the first.
func TestIndexSchemasByFeatureRejectsDuplicateClaim(t *testing.T) {
	byName := map[string]SchemaDocument{
		"Foo.json":        {File: "schemas/base/Foo.json", Shape: SchemaShape{FilenameShape: "request-bare"}},
		"FooRequest.json": {File: "schemas/security/FooRequest.json", Shape: SchemaShape{FilenameShape: "request-suffixed"}},
	}
	_, _, err := indexSchemasByFeature(byName)
	if err == nil {
		t.Fatal("indexSchemasByFeature did not reject two files claiming the same request side of Foo")
	}
}

// TestIndexSchemasByFeatureReproducesV201Pairing is a same-convention
// regression guard: a corpus that is uniformly request-suffixed (2.0.1's own
// shape) must resolve exactly the way the original featureName+"Request.json"
// concatenation did, so this generalization changes no 2.0.1 output.
func TestIndexSchemasByFeatureReproducesV201Pairing(t *testing.T) {
	byName := map[string]SchemaDocument{
		"AuthorizeRequest.json":  {File: "AuthorizeRequest.json", Shape: SchemaShape{FilenameShape: "request-suffixed"}},
		"AuthorizeResponse.json": {File: "AuthorizeResponse.json", Shape: SchemaShape{FilenameShape: "response"}},
	}
	requests, responses, err := indexSchemasByFeature(byName)
	if err != nil {
		t.Fatalf("indexSchemasByFeature: %v", err)
	}
	if requests["Authorize"].File != "AuthorizeRequest.json" || responses["Authorize"].File != "AuthorizeResponse.json" {
		t.Fatalf("requests=%v responses=%v, want the literal featureName+\"Request/Response.json\" pairing preserved", requests, responses)
	}
}
