package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeCandidateSnapshot struct {
	AnsweredExclusion      *tracker.NativeAnsweredExclusion
	StateExclusion         string
	IDs                    []tracker.WorkItemID
	UnresolvedDependencies []tracker.NativeDependency
	Unavailable            []string
}

func nativeCandidateValues[T ~string | ~int64](values []T) []any {
	args := make([]any, len(values))
	for index, value := range values {
		args[index] = value
	}
	return args
}
