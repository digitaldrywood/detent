package provenance

import (
	"encoding/json"
	"strings"
)

type Origin string

const (
	OriginHuman      Origin = "human"
	OriginRoutine    Origin = "routine"
	OriginRetro      Origin = "retro"
	OriginDependency Origin = "dependency"
	OriginAdmission  Origin = "admission"
	OriginUnknown    Origin = "unknown"
)

type Actor struct {
	Login string `json:"login,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

type Attribution struct {
	Origin Origin `json:"origin"`
	Actor  *Actor `json:"actor,omitempty"`
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
	case OriginHuman, OriginRoutine, OriginRetro, OriginDependency, OriginAdmission, OriginUnknown:
		return origin
	default:
		return OriginUnknown
	}
}

func OriginFromActor(actor Actor) Origin {
	if strings.TrimSpace(actor.Login) != "" && strings.EqualFold(strings.TrimSpace(actor.Kind), "user") {
		return OriginHuman
	}
	return OriginUnknown
}

func Apply(raw string, attribution Attribution, admission *Admission) string {
	fields := map[string]json.RawMessage{}
	if trimmed := strings.TrimSpace(raw); trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
			fields = map[string]json.RawMessage{}
		}
	}
	attribution.Origin = NormalizeOrigin(attribution.Origin)
	if attribution.Actor != nil {
		actor := Actor{
			Login: strings.TrimSpace(attribution.Actor.Login),
			Kind:  strings.TrimSpace(attribution.Actor.Kind),
		}
		if actor.Login == "" && actor.Kind == "" {
			attribution.Actor = nil
		} else {
			attribution.Actor = &actor
		}
	}
	provenanceJSON, err := json.Marshal(attribution)
	if err != nil {
		return `{"provenance":{"origin":"unknown"}}`
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
		return `{"provenance":{"origin":"unknown"}}`
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
	return metadata, true
}
