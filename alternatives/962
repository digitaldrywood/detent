package failureattribution

import (
	"strings"
	"testing"
)

func TestAttributionRejectsUnboundOrIncompleteProof(t *testing.T) {
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	head := Observation{Command: "check-input", HeadSHA: headSHA, ExitCode: 1, Output: "source.go:4: invalid input\n", Complete: true}
	base := head
	base.HeadSHA = baseSHA
	for _, test := range []struct {
		name       string
		edit       func(*Observation, *Observation)
		diagnostic string
	}{
		{name: "wrong base", edit: func(_, b *Observation) { b.HeadSHA = headSHA }},
		{name: "wrong head", edit: func(h, _ *Observation) { h.HeadSHA = baseSHA }},
		{name: "different command", edit: func(_, b *Observation) { b.Command = "another-check" }},
		{name: "head output truncated", edit: func(h, _ *Observation) { h.Complete = false }},
		{name: "base output truncated", edit: func(_, b *Observation) { b.Complete = false }},
		{name: "unrelated base failure", edit: func(_, b *Observation) { b.Output = "another failure\n" }},
		{name: "missing head diagnostic", edit: func(h, _ *Observation) { h.Output = "another failure\n" }},
		{name: "command unavailable", edit: func(_, b *Observation) { b.ExitCode = 127 }},
		{name: "diagnostic prefix is insufficient", diagnostic: "source.go:4:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, b := head, base
			if test.edit != nil {
				test.edit(&h, &b)
			}
			diagnostic := test.diagnostic
			if diagnostic == "" {
				diagnostic = "source.go:4: invalid input"
			}
			got, evidence := Classify(baseSHA, headSHA, diagnostic, h, b)
			if got != Unknown || evidence != nil {
				t.Fatalf("unproven finding = %s %#v", got, evidence)
			}
		})
	}
}
