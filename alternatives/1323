package nativechange

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/policy"
)

func clientTestPolicy() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}
