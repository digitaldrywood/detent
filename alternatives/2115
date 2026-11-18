package hubapi

import (
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type Error struct {
	Code            string           `json:"code"`
	Message         string           `json:"message"`
	CurrentRevision tracker.Revision `json:"current_revision,string,omitempty"`
	Details         map[string]any   `json:"details,omitempty"`
	Status          int              `json:"-"`
	Public          bool             `json:"-"`
	logAttributes   []any
}

func (e *Error) Error() string {
	if e.Public {
		return e.Message
	}
	return e.Code
}

func (e *Error) LogAttributes() []any {
	return append([]any(nil), e.logAttributes...)
}

func Invalid(message string) error {
	return &Error{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity}
}

func PublicInvalid(message string) error {
	return &Error{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity, Public: true}
}

func NotFound() error {
	_, file, line, _ := runtime.Caller(1)
	return &Error{Code: "not_found", Message: "Resource was not found", Status: http.StatusNotFound, Public: true, logAttributes: []any{"not_found_source", filepath.Base(file) + ":" + strconv.Itoa(line)}}
}

func Conflict(revision tracker.Revision) error {
	return &Error{Code: "revision_conflict", Message: "Resource has changed", CurrentRevision: revision, Status: http.StatusConflict, Public: true}
}

func WorkItemStorageUnavailable() error {
	return &Error{Code: "not_ready", Message: "This work item operation requires storage that is not available", Status: http.StatusServiceUnavailable, Public: true}
}

func RunEventStorageUnavailable() error {
	return &Error{Code: "not_ready", Message: "Run event storage is not available", Status: http.StatusServiceUnavailable, Public: true}
}

func (e *Error) WithLogAttributes(attributes ...any) *Error {
	copied := *e
	copied.logAttributes = append([]any(nil), attributes...)
	return &copied
}
