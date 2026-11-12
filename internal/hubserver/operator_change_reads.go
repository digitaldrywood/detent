package hubserver

import (
	"encoding/base64"
	"encoding/json"
	"slices"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeReadCursor struct {
	Offset  int64  `json:"offset,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Change  string `json:"change"`
	Head    string `json:"head"`
	Version string `json:"version"`
	Section string `json:"section"`
	After   int64  `json:"after"`
	Through int64  `json:"through"`
}

func validChangeReadSection(section string) bool {
	return section == "current" || section == "versions" || section == "reviews" || section == "checks" || section == "discussion"
}

func encodeChangeReadCursor(cursor changeReadCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw), err
}

func boundedChangeReviewResult(result *gate.ValidatorResult) *gate.ValidatorResult {
	if result == nil {
		return nil
	}
	bounded := *result
	bounded.Commands = slices.Clone(bounded.Commands)
	for i := range bounded.Commands {
		bounded.Commands[i] = *boundedChangeCommand(&bounded.Commands[i])
	}
	return &bounded
}

func boundedChangeCommand(command *gate.CommandResult) *gate.CommandResult {
	if command == nil {
		return nil
	}
	result := tracker.PublicGateResult(*command)
	return &result
}

func boundedChangeLanding(landing *tracker.NativeLandingReceipt) *tracker.NativeLandingReceipt {
	if landing == nil {
		return nil
	}
	result := *landing
	result.Gate = boundedChangeCommand(result.Gate)
	return &result
}

func boundedChangeVersion(version tracker.ChangeVersion) tracker.ChangeVersion {
	if version.Policy.Authored != nil {
		authored := *version.Policy.Authored
		authored.Files = nil
		version.Policy.Authored = &authored
	}
	version.Policy.Configuration = nil
	version.Policy.Workflow = nil
	version.Validation = boundedChangeCommand(version.Validation)
	version.Landing = boundedChangeLanding(version.Landing)
	return version
}
