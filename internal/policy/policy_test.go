package policy

import (
	"reflect"
	"strings"
	"testing"
)

func testDescriptor() Descriptor {
	return Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: Digest([]byte("source")), ConfigDigest: Digest([]byte("config")),
		Gates: Gates{Kind: "command", PlanReview: "human", PlanStopDigest: Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

func TestDescriptorValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*Descriptor)
		valid  bool
	}{
		{"valid", func(*Descriptor) {}, true},
		{"schema", func(d *Descriptor) { d.Schema++ }, false},
		{"forged identity", func(d *Descriptor) { d.Gates.Kind = "human_review" }, false},
		{"missing source", func(d *Descriptor) { d.SourceDigest = ""; *d = d.WithID() }, false},
		{"nonrevision prose", func(d *Descriptor) { d.SourceRevision = "private prose"; *d = d.WithID() }, false},
		{"profile path", func(d *Descriptor) { d.Profile = "/private/config"; *d = d.WithID() }, false},
		{"tag command", func(d *Descriptor) { d.Requirements.RequiredTags = []string{"run make check"}; *d = d.WithID() }, false},
		{"gate command", func(d *Descriptor) { d.Gates.Kind = "make check"; *d = d.WithID() }, false},
		{"check bound", func(d *Descriptor) { d.Gates.RequiredChecks = 257; *d = d.WithID() }, false},
		{"plan stop", func(d *Descriptor) { d.Gates.PlanStopDigest = "Private Stop"; *d = d.WithID() }, false},
		{"plan review", func(d *Descriptor) { d.Gates.PlanReview = "shell"; *d = d.WithID() }, false},
		{"merge", func(d *Descriptor) { d.Gates.MergeMethod = "custom-command"; *d = d.WithID() }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := testDescriptor()
			test.change(&d)
			if err := d.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate() = %v, want valid %t", err, test.valid)
			}
		})
	}
}

func TestRequirements(t *testing.T) {
	t.Parallel()
	r := Requirements{RequiredTags: []string{" Linux ", "linux", "gpu"}, RunnerID: "runner_abc", MachineID: "machine_abc"}
	normalized := r.Normalized()
	if !reflect.DeepEqual(normalized.RequiredTags, []string{"gpu", "linux"}) || r.RequiredTags[0] != " Linux " {
		t.Fatalf("normalization mutated input or failed to deduplicate: %#v %#v", r, normalized)
	}
	for _, test := range []struct {
		name            string
		requirements    Requirements
		runner, machine string
		tags            []string
		valid           bool
	}{
		{"empty", Requirements{}, "", "", nil, true},
		{"all constraints", normalized, "runner_abc", "machine_abc", []string{"linux", "gpu"}, true},
		{"missing tag", normalized, "runner_abc", "machine_abc", []string{"linux"}, false},
		{"wrong runner", normalized, "runner_other", "machine_abc", []string{"linux", "gpu"}, false},
		{"wrong machine", normalized, "runner_abc", "machine_other", []string{"linux", "gpu"}, false},
		{"display name", Requirements{RunnerID: "Build Mac"}, "Build Mac", "", nil, false},
		{"bare prefix", Requirements{MachineID: "machine_"}, "", "machine_", nil, false},
		{"invalid tag", Requirements{RequiredTags: []string{"bad/tag"}}, "", "", nil, false},
		{"too many tags", Requirements{RequiredTags: make([]string, 33)}, "", "", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.requirements.Match(test.runner, test.machine, test.tags); (err == nil) != test.valid {
				t.Fatalf("Match() = %v, want valid %t", err, test.valid)
			}
		})
	}
}

func TestDescriptorMatch(t *testing.T) {
	t.Parallel()
	d := testDescriptor()
	other := d
	other.Gates.AutoPromote = true
	other = other.WithID()
	for _, test := range []struct {
		name             string
		actual, expected Descriptor
		valid            bool
	}{
		{"same", d, d, true}, {"untrusted relaxation", other, d, false},
		{"missing actual", Descriptor{}, d, false}, {"missing expected", d, Descriptor{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.actual.Match(test.expected); (err == nil) != test.valid {
				t.Fatalf("Match() = %v, want valid %t", err, test.valid)
			}
		})
	}
}
