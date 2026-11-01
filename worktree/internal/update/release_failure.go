package update

import (
	"errors"
	"strings"
)

const workerReleasePreparationFailure = "Execution release preparation failed"

func workerReleaseFailureReason(err error) string {
	detail := safeReleaseFailureDetail(err)
	if detail == "" {
		return workerReleasePreparationFailure
	}
	return workerReleasePreparationFailure + ": " + detail
}

func safeReleaseFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	const unknownField = `json: unknown field "`
	if _, suffix, found := strings.Cut(message, unknownField); found {
		field, _, closed := strings.Cut(suffix, `"`)
		if closed && safeProvenanceField(field) {
			return `release provenance contains unsupported field "` + field + `"`
		}
		return "release provenance contains an unsupported field"
	}
	switch {
	case strings.Contains(message, "release provenance has no mandatory check evidence"):
		return "release provenance has no mandatory check evidence"
	case strings.Contains(message, "release provenance contains an unnamed mandatory check"):
		return "release provenance contains an unnamed mandatory check"
	case strings.Contains(message, "release provenance contains duplicate mandatory check"):
		return "release provenance contains duplicate mandatory check evidence"
	case strings.Contains(message, "release provenance mandatory check"):
		return "release provenance mandatory check evidence is invalid"
	case strings.Contains(message, "release provenance schema ="):
		return "release provenance schema is unsupported"
	case strings.Contains(message, "release provenance repository ="):
		return "release provenance repository does not match"
	case strings.Contains(message, "release provenance tag ="):
		return "release provenance tag does not match"
	case strings.Contains(message, "release provenance commit ="):
		return "release provenance commit does not match"
	case strings.Contains(message, "release provenance commit:"):
		return "release provenance commit is invalid"
	case strings.Contains(message, "decode release provenance:"):
		return "release provenance JSON is invalid"
	case errors.Is(err, ErrWorkerProtocolIncompatible):
		return "execution worker protocol is incompatible with the controller"
	default:
		return ""
	}
}

func safeProvenanceField(field string) bool {
	if len(field) == 0 || len(field) > 64 || field[0] < 'A' || field[0] > 'Z' && field[0] < 'a' || field[0] > 'z' {
		return false
	}
	for _, char := range field[1:] {
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' {
			continue
		}
		return false
	}
	return true
}
