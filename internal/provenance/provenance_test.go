package provenance

import (
	"encoding/json"
	"testing"
)

func TestAttributionFromSource(t *testing.T) {
	t.Parallel()

	operator := Actor{Login: "corylanou", Kind: "User"}
	tests := []struct {
		name          string
		source        Source
		actor         Actor
		wantOrigin    Origin
		wantInitiator Initiator
		wantBasis     Basis
	}{
		{name: "Detent instance", source: SourceDetentInstance, wantOrigin: OriginDetent, wantInitiator: InitiatorDetentInstance, wantBasis: BasisDetentOperation},
		{name: "Detent agent using a user token", source: SourceDetentAgentSession, actor: operator, wantOrigin: OriginAgent, wantInitiator: InitiatorDetentAgentSession, wantBasis: BasisActiveAgentSession},
		{name: "external automation using an operator token", source: SourceExternalAutomation, actor: operator, wantOrigin: OriginAutomation, wantInitiator: InitiatorExternalAutomation, wantBasis: BasisExplicitAutomation},
		{name: "authenticated human", source: SourceHumanSession, actor: operator, wantOrigin: OriginHuman, wantInitiator: InitiatorHuman, wantBasis: BasisAuthenticatedHuman},
		{name: "unverified user tracker actor", source: SourceTrackerObservation, actor: operator, wantOrigin: OriginIndeterminate, wantInitiator: InitiatorIndeterminate, wantBasis: BasisTrackerActor},
		{name: "verified bot tracker actor", source: SourceTrackerObservation, actor: Actor{Login: "dependabot[bot]", Kind: "Bot"}, wantOrigin: OriginAutomation, wantInitiator: InitiatorExternalAutomation, wantBasis: BasisTrackerActor},
		{name: "missing tracker actor", source: SourceTrackerObservation, wantOrigin: OriginIndeterminate, wantInitiator: InitiatorIndeterminate, wantBasis: BasisTrackerActor},
		{name: "unknown source", actor: operator, wantOrigin: OriginIndeterminate, wantInitiator: InitiatorIndeterminate, wantBasis: BasisUnspecified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := AttributionFromSource(tt.source, tt.actor)
			if got.Schema != CurrentSchema || got.Origin != tt.wantOrigin || got.Initiator != tt.wantInitiator || got.Basis != tt.wantBasis {
				t.Fatalf("AttributionFromSource(%q, %#v) = %#v, want schema %d origin %q initiator %q basis %q", tt.source, tt.actor, got, CurrentSchema, tt.wantOrigin, tt.wantInitiator, tt.wantBasis)
			}
			if got.Origin == OriginHuman && tt.source != SourceHumanSession {
				t.Fatalf("AttributionFromSource(%q, %#v) fell back to human", tt.source, tt.actor)
			}
		})
	}
}

func TestOriginFromActorNeverInfersHuman(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		actor Actor
		want  Origin
	}{
		{name: "user", actor: Actor{Login: "ada", Kind: "User"}, want: OriginIndeterminate},
		{name: "bot", actor: Actor{Login: "dependabot[bot]", Kind: "Bot"}, want: OriginAutomation},
		{name: "missing kind", actor: Actor{Login: "ada"}, want: OriginIndeterminate},
		{name: "missing actor", want: OriginIndeterminate},
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
		AttributionFromSource(SourceDetentInstance, Actor{Login: "detent", Kind: "App"}),
		&Admission{ProposalID: "proposal-1", Attributed: true},
	)
	metadata, ok := Parse(raw)
	if !ok {
		t.Fatalf("Parse(%q) = false", raw)
	}
	if metadata.Provenance.Schema != CurrentSchema ||
		metadata.Provenance.Origin != OriginDetent ||
		metadata.Provenance.Initiator != InitiatorDetentInstance ||
		metadata.Provenance.Actor == nil ||
		metadata.Provenance.Actor.Login != "detent" ||
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

func TestParsePreservesLegacyAttribution(t *testing.T) {
	t.Parallel()

	metadata, ok := Parse(`{"provenance":{"origin":"human","actor":{"login":"corylanou","kind":"User"}}}`)
	if !ok {
		t.Fatal("Parse() = false")
	}
	if metadata.Provenance.Schema != 0 || metadata.Provenance.Origin != OriginHuman || metadata.Provenance.Initiator != "" {
		t.Fatalf("legacy provenance = %#v, want unchanged legacy human record", metadata.Provenance)
	}
}
