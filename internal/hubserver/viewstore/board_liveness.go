package viewstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type BoardLiveness struct {
	AttemptID      string
	ExecutionID    string
	LeaseExpiresAt *time.Time
	AttemptUntil   *time.Time
	ExecutionUntil *time.Time
}

func (l BoardLiveness) Apply(a *tracker.NativeBoardAttempt, e *tracker.NativeBoardExecution, now time.Time) (*tracker.NativeBoardAttempt, *tracker.NativeBoardExecution, *time.Time) {
	if l.LeaseExpiresAt == nil || !now.Before(*l.LeaseExpiresAt) {
		l = BoardLiveness{}
	}
	var deadline *time.Time
	if a != nil && a.Status == "running" {
		attempt := *a
		if a.ID != l.AttemptID || l.AttemptUntil == nil || !now.Before(*l.AttemptUntil) {
			attempt.Status = "interrupted"
		} else {
			attempt.ExpiresAt = *l.LeaseExpiresAt
			deadline = l.AttemptUntil
		}
		a = &attempt
	}
	if e != nil && (e.Running || e.Stage != "") {
		execution := *e
		if e.ID != l.ExecutionID || l.ExecutionUntil == nil || !now.Before(*l.ExecutionUntil) {
			if e.Running && deadline != nil {
				return a, nil, deadline
			}
			execution.Running, execution.Stage = false, ""
		} else {
			execution.ExpiresAt = *l.LeaseExpiresAt
			if deadline == nil || l.ExecutionUntil.Before(*deadline) {
				deadline = l.ExecutionUntil
			}
		}
		e = &execution
	}
	return a, e, deadline
}
