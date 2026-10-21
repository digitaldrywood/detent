package operatortool

import "github.com/digitaldrywood/detent/internal/mutation"

type ConflictError struct {
	Code            string           `json:"code"`
	Message         string           `json:"message,omitempty"`
	CurrentRevision int64            `json:"current_revision,string,omitempty"`
	Details         *ConflictDetails `json:"details,omitempty"`
}

type ConflictDetails struct {
	ExpectedAttemptID *string `json:"expected_attempt_id"`
	CurrentAttemptID  *string `json:"current_attempt_id"`
}

func (e *ConflictError) Error() string { return e.Code }

func (e *ConflictError) Unwrap() error { return mutation.ErrConflict }
