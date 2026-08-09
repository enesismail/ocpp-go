package main

import "testing"

// TestSnakeCaseSplitsOnCaseAndDigitBoundaries covers the rule that turns a
// message name into the name of its generated file. snakeCase must split
// words the same way splitWords already does for identifiers — at a
// lowercase-to-uppercase transition, at the last uppercase letter before a
// following lowercase run inside an all-caps stretch, and at a digit/non-digit
// transition — rather than treating every uppercase letter as its own word
// boundary and ignoring digit boundaries entirely. Get15118EVCertificate,
// NotifyEVChargingNeeds and NotifyEVChargingSchedule are the three names, out
// of all 64 declared in config/v201.yaml, where a per-rune uppercase-only scan
// disagrees with the splitWords-based rule: it fragments the "EV" initialism
// into two single-letter words and folds a digit run into the following word
// instead of splitting it off.
func TestSnakeCaseSplitsOnCaseAndDigitBoundaries(t *testing.T) {
	cases := []struct {
		name string
		want string
		note string
	}{
		{name: "Get15118EVCertificate", want: "get_15118_ev_certificate", note: "digit run followed by a two-letter initialism"},
		{name: "NotifyEVChargingNeeds", want: "notify_ev_charging_needs", note: "two-letter initialism ahead of a title-cased word"},
		{name: "NotifyEVChargingSchedule", want: "notify_ev_charging_schedule", note: "same initialism boundary, different tail word"},
		// Adversarial cases beyond the three real message names above,
		// probing the same boundary rules in shapes the v201 corpus never
		// happens to exercise.
		{name: "3PhaseError", want: "3_phase_error", note: "a digit run at the very start of the name"},
		{name: "AVersion", want: "a_version", note: "a single-letter word ahead of a title-cased word"},
		{name: "OCSPRequestDataType", want: "ocsp_request_data_type", note: "a four-letter all-caps initialism run ahead of three title-cased words"},
	}
	for _, tc := range cases {
		t.Run(tc.note, func(t *testing.T) {
			if got := snakeCase(tc.name); got != tc.want {
				t.Fatalf("snakeCase(%q) = %q, want %q (%s)", tc.name, got, tc.want, tc.note)
			}
		})
	}
}
