package issueorigin

import (
	"strings"
	"testing"
)

func TestOrigin(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"routine", "lesson", "worker", "doctor", "audit"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			want := Origin{Kind: kind, Instance: "instance-1", Source: "run-2", Fingerprint: "problem"}
			body := Stamp("Finding", want)
			got, ok := Parse(body)
			if !ok || got != want || Marker(body) != kind {
				t.Fatalf("Parse() = %+v, %v; marker = %s", got, ok, Marker(body))
			}
			if got := Preserve("Updated", body); !strings.Contains(got, "Updated") || !strings.Contains(got, "instance-1") {
				t.Fatalf("Preserve() = %s", got)
			}
			if got := Stamp(body, want); got != body {
				t.Fatalf("Stamp is not idempotent: %s", got)
			}
		})
	}
}

func TestLegacyAndInvalidOrigins(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body, kind, fingerprint string }{
		{"operator", "An operator request", "operator", ""},
		{"legacy colon", "<!-- detent-audit-fp:disk-capacity -->", "audit", "disk-capacity"},
		{"legacy equals", "<!-- detent-audit-fp=disk-capacity -->", "audit", "disk-capacity"},
		{"invalid", "```detent-origin\norigin_kind: unknown\nfingerprint: x\n```", "operator", ""},
		{"missing fingerprint", "```detent-origin\norigin_kind: routine\n```", "operator", ""},
		{"malformed", "```detent-origin\n[\n```", "operator", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			origin, _ := Parse(tt.body)
			if origin.Fingerprint != tt.fingerprint || Marker(tt.body) != tt.kind {
				t.Fatalf("origin = %+v, marker = %s", origin, Marker(tt.body))
			}
		})
	}
	if Fingerprint("  Disk CAPACITY ") != Fingerprint("disk capacity") || Fingerprint("disk capacity") == Fingerprint("network") {
		t.Fatal("fingerprint normalization failed")
	}
	body := Stamp("<!-- detent-audit-fp:legacy -->", Origin{Kind: "worker", Source: "attempt"})
	origin, ok := Parse(body)
	if !ok || origin.Instance == "" || origin.Fingerprint != "legacy" {
		t.Fatalf("legacy stamp = %+v", origin)
	}
	if Preserve("updated", "operator body") != "updated" || !strings.Contains(Occurrence(body), "New machine occurrence") {
		t.Fatal("body preservation or occurrence failed")
	}
}
