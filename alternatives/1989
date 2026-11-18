package healthsignals

import (
	"strings"
	"time"
)

type Deps struct{ NormalizeFailureSignature func(string) string }
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) RecoveryReason(text string) string {
	_, reason, found := strings.Cut(text, "native checkpoint requires recovery: ")
	if !found {
		return ""
	}
	return s.deps.NormalizeFailureSignature(reason)
}

type ResidenceIssue struct {
	ID      string
	Partial bool
}
type ResidenceSpan struct {
	Lane     string
	From, To time.Time
}
type ResidenceTimeline struct {
	Issue   ResidenceIssue
	Spans   []ResidenceSpan
	State   string
	Entered time.Time
	Done    []time.Time
}
