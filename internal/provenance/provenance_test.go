package provenance

import (
	"encoding/json"
	"testing"
)

func TestOriginFromActorRequiresExplicitUserType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		actor Actor
		want  Origin
	}{
		{name: "user", actor: Actor{Login: "ada", Kind: "User"}, want: OriginHuman},
		{name: "bot", actor: Actor{Login: "dependabot[bot]", Kind: "Bot"}, want: OriginUnknown},
		{name: "missing kind", actor: Actor{Login: "ada"}, want: OriginUnknown},
		{name: "missing actor", want: OriginUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := OriginFromActor(tt.actor); got != tt.want {
				t.Fatalf("OriginFromActor(%#v) = %q, want %q", tt.actor, got, tt.want)
			}
		})
	}
}

func TestApplyPreservesMetadataAndRecordsAdmission(t *testing.T) {
	t.Parallel()

	raw := Apply(
		`{"pull_request":{"number":1537},"provenance":{"origin":"unknown"}}`,
		Attribution{
			Origin: OriginAdmission,
			Actor:  &Actor{Login: "ada", Kind: "User"},
		},
		&Admission{ProposalID: "proposal-1", Attributed: true},
	)
	metadata, ok := Parse(raw)
	if !ok {
		t.Fatalf("Parse(%q) = false", raw)
	}
	if metadata.Provenance.Origin != OriginAdmission ||
		metadata.Provenance.Actor == nil ||
		metadata.Provenance.Actor.Login != "ada" ||
		metadata.Admission == nil ||
		metadata.Admission.ProposalID != "proposal-1" ||
		!metadata.Admission.Attributed {
		t.Fatalf("metadata = %#v", metadata)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := fields["pull_request"]; !ok {
		t.Fatalf("Apply() removed existing metadata: %s", raw)
	}
}
