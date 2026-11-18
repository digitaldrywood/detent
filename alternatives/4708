package provenance

import (
	"encoding/json"
	"strings"
)

const CurrentSchema = 2

type Origin string

const (
	OriginHuman         Origin = "human"
	OriginDetent        Origin = "detent"
	OriginAgent         Origin = "agent"
	OriginAutomation    Origin = "external_automation"
	OriginIndeterminate Origin = "indeterminate"
	OriginRoutine       Origin = "routine"
	OriginRetro         Origin = "retro"
	OriginDependency    Origin = "dependency"
	OriginAdmission     Origin = "admission"
	OriginUnknown       Origin = "unknown"
)

type Initiator string

const (
	InitiatorDetentInstance     Initiator = "detent_instance"
	InitiatorDetentAgentSession Initiator = "detent_agent_session"
	InitiatorExternalAutomation Initiator = "external_automation"
	InitiatorHuman              Initiator = "human"
	InitiatorIndeterminate      Initiator = "indeterminate"
)

type Source string

const (
	SourceDetentInstance     Source = "detent_instance"
	SourceDetentAgentSession Source = "detent_agent_session"
	SourceExternalAutomation Source = "external_automation"
	SourceHumanSession       Source = "human_session"
	SourceTrackerObservation Source = "tracker_observation"
)

type Basis string

const (
	BasisDetentOperation    Basis = "detent_operation"
	BasisActiveAgentSession Basis = "active_agent_session"
	BasisExplicitAutomation Basis = "explicit_automation"
	BasisAuthenticatedHuman Basis = "authenticated_human_session"
	BasisTrackerActor       Basis = "tracker_actor"
	BasisUnspecified        Basis = "unspecified"
)

type Actor struct {
	Login string `json:"login,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

type Attribution struct {
	Schema    int       `json:"schema,omitempty"`
	Origin    Origin    `json:"origin"`
	Initiator Initiator `json:"initiator,omitempty"`
	Basis     Basis     `json:"basis,omitempty"`
	Actor     *Actor    `json:"actor,omitempty"`
}

type Admission struct {
	ProposalID string `json:"proposal_id,omitempty"`
	Attributed bool   `json:"attributed"`
}

type Metadata struct {
	Provenance Attribution `json:"provenance"`
	Admission  *Admission  `json:"admission,omitempty"`
}

func NormalizeOrigin(origin Origin) Origin {
	switch origin {
	case OriginHuman, OriginDetent, OriginAgent, OriginAutomation, OriginIndeterminate,
		OriginRoutine, OriginRetro, OriginDependency, OriginAdmission, OriginUnknown:
		return origin
	default:
		return OriginUnknown
	}
}

func AttributionFromSource(source Source, actor Actor) Attribution {
	attribution := Attribution{
		Schema: CurrentSchema,
		Actor:  actorPointer(actor),
	}
	switch source {
	case SourceDetentInstance:
		attribution.Origin = OriginDetent
		attribution.Initiator = InitiatorDetentInstance
		attribution.Basis = BasisDetentOperation
	case SourceDetentAgentSession:
		attribution.Origin = OriginAgent
		attribution.Initiator = InitiatorDetentAgentSession
		attribution.Basis = BasisActiveAgentSession
	case SourceExternalAutomation:
		attribution.Origin = OriginAutomation
		attribution.Initiator = InitiatorExternalAutomation
		attribution.Basis = BasisExplicitAutomation
	case SourceHumanSession:
		attribution.Origin = OriginHuman
		attribution.Initiator = InitiatorHuman
		attribution.Basis = BasisAuthenticatedHuman
	case SourceTrackerObservation:
		attribution.Basis = BasisTrackerActor
		if actorIsAutomation(actor) {
			attribution.Origin = OriginAutomation
			attribution.Initiator = InitiatorExternalAutomation
		} else {
			attribution.Origin = OriginIndeterminate
			attribution.Initiator = InitiatorIndeterminate
		}
	default:
		attribution.Origin = OriginIndeterminate
		attribution.Initiator = InitiatorIndeterminate
		attribution.Basis = BasisUnspecified
	}
	return attribution
}

func OriginFromActor(actor Actor) Origin {
	return AttributionFromSource(SourceTrackerObservation, actor).Origin
}

func Prepare(attribution Attribution) Attribution {
	attribution.Schema = CurrentSchema
	attribution.Origin = NormalizeOrigin(attribution.Origin)
	attribution.Actor = actorPointerValue(attribution.Actor)
	if attribution.Initiator == "" {
		attribution.Initiator = initiatorForOrigin(attribution.Origin)
	}
	if attribution.Basis == "" {
		attribution.Basis = BasisUnspecified
	}
	if attribution.Origin == OriginUnknown {
		attribution.Origin = OriginIndeterminate
		attribution.Initiator = InitiatorIndeterminate
	}
	return attribution
}

func Apply(raw string, attribution Attribution, admission *Admission) string {
	fields := map[string]json.RawMessage{}
	if trimmed := strings.TrimSpace(raw); trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
			fields = map[string]json.RawMessage{}
		}
	}
	attribution = Prepare(attribution)
	provenanceJSON, err := json.Marshal(attribution)
	if err != nil {
		return indeterminateMetadata
	}
	fields["provenance"] = provenanceJSON
	if admission != nil {
		value := Admission{
			ProposalID: strings.TrimSpace(admission.ProposalID),
			Attributed: admission.Attributed,
		}
		admissionJSON, err := json.Marshal(value)
		if err == nil {
			fields["admission"] = admissionJSON
		}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return indeterminateMetadata
	}
	return string(data)
}

func Parse(raw string) (Metadata, bool) {
	var metadata Metadata
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &metadata); err != nil {
		return Metadata{}, false
	}
	metadata.Provenance.Origin = NormalizeOrigin(metadata.Provenance.Origin)
	if metadata.Provenance.Origin == OriginUnknown && !strings.Contains(raw, `"origin"`) {
		return Metadata{}, false
	}
	metadata.Provenance.Actor = actorPointerValue(metadata.Provenance.Actor)
	return metadata, true
}

const indeterminateMetadata = `{"provenance":{"schema":2,"origin":"indeterminate","initiator":"indeterminate","basis":"unspecified"}}`

func initiatorForOrigin(origin Origin) Initiator {
	switch origin {
	case OriginHuman:
		return InitiatorHuman
	case OriginAgent:
		return InitiatorDetentAgentSession
	case OriginAutomation:
		return InitiatorExternalAutomation
	case OriginDetent, OriginRoutine, OriginRetro, OriginDependency, OriginAdmission:
		return InitiatorDetentInstance
	default:
		return InitiatorIndeterminate
	}
}

func actorIsAutomation(actor Actor) bool {
	switch strings.ToLower(strings.TrimSpace(actor.Kind)) {
	case "bot", "app":
		return strings.TrimSpace(actor.Login) != ""
	default:
		return false
	}
}

func actorPointer(actor Actor) *Actor {
	actor.Login = strings.TrimSpace(actor.Login)
	actor.Kind = strings.TrimSpace(actor.Kind)
	if actor.Login == "" && actor.Kind == "" {
		return nil
	}
	return &actor
}

func actorPointerValue(actor *Actor) *Actor {
	if actor == nil {
		return nil
	}
	return actorPointer(*actor)
}
