package hubapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func FromFeatureError(err error) error {
	var failure *Error
	if !errors.As(err, &failure) || failure == nil {
		return err
	}
	return &NativeError{Code: failure.Code, Message: failure.Message, CurrentRevision: failure.CurrentRevision, Details: failure.Details, Status: failure.Status, PublicMessage: failure.Public, LogAttributes: failure.LogAttributes()}
}

func ToFeatureError(err error) error {
	var native *NativeError
	if !errors.As(err, &native) || native == nil {
		return err
	}
	failure := &Error{Code: native.Code, Message: native.Message, CurrentRevision: native.CurrentRevision, Details: native.Details, Status: native.Status, Public: native.PublicMessage}
	return failure.WithLogAttributes(native.LogAttributes...)
}

func FeatureResult[T any](value T, err error) (T, error) {
	return value, FromFeatureError(err)
}

const NativeBase = "/api/v2/organizations/:organization/projects/:project"

type NativeError struct {
	Code            string           `json:"code"`
	Message         string           `json:"message"`
	CurrentRevision tracker.Revision `json:"current_revision,string,omitempty"`
	Details         map[string]any   `json:"details,omitempty"`
	Status          int              `json:"-"`
	PublicMessage   bool             `json:"-"`
	LogAttributes   []any            `json:"-"`
	RetryAfter      int              `json:"-"`
}

func (e *NativeError) Error() string {
	if e.PublicMessage {
		return e.Message
	}
	return e.Code
}

func NativeInvalid(message string) error {
	return &NativeError{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity}
}

func NativePublicInvalid(message string) error {
	return &NativeError{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

func NativeNotFound() error {
	_, file, line, _ := runtime.Caller(1)
	return &NativeError{Code: "not_found", Message: "Resource was not found", Status: http.StatusNotFound, PublicMessage: true, LogAttributes: []any{"not_found_source", filepath.Base(file) + ":" + strconv.Itoa(line)}}
}

func NativeNotFoundAt(step string) error {
	return &NativeError{Code: "not_found", Message: "Resource was not found", Status: http.StatusNotFound, PublicMessage: true, LogAttributes: []any{"not_found_step", step}}
}

func NativeConflict(revision tracker.Revision) error {
	return &NativeError{Code: "revision_conflict", Message: "Resource has changed", CurrentRevision: revision, Status: http.StatusConflict, PublicMessage: true}
}

// IsNativeNotFound reports whether err is the opaque native not-found error.
func IsNativeNotFound(err error) bool {
	err = FromFeatureError(err)
	var failure *NativeError
	return errors.As(err, &failure) && failure != nil && failure.Code == "not_found"
}

func IsNativeConflict(err error) bool {
	err = FromFeatureError(err)
	var failure *NativeError
	return errors.As(err, &failure) && failure != nil && failure.Code == "revision_conflict"
}

func (credential APICredential) ViaKey() string {
	if credential.EntryKey != nil {
		return credential.EntryKey.ID
	}
	if credential.HostedKeyScope != "" {
		return credential.ID
	}
	return ""
}

func MarshalNative(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

type NativeCommandOptions struct {
	RunnerAdministration                     bool
	OperationID, Item, Feature               string
	ChangeID, VersionID                      string
	CheckPrincipal, RequireLease, Completion bool
	ArtifactRead                             bool
}

type NativeArchiveError struct {
	Archive *tracker.ArchiveDescriptor `json:"archive"`
}

func (e *NativeArchiveError) Error() string { return "archive section " + e.Archive.Availability }

func NativeArchiveReadError(err error) *operatortool.RequestError {
	var unavailable *NativeArchiveError
	if !errors.As(err, &unavailable) {
		return nil
	}
	return &operatortool.RequestError{Code: "archive_" + unavailable.Archive.Availability, Message: unavailable.Error(), Archive: unavailable.Archive}
}

// NativeStaleExecution is the conversation contract's stale_execution: the expected owner
// generation is no longer the current one. It lives here rather than beside
// one of its callers because every path that is fenced by an ownership
// generation has to report the same code for the same reason.
func NativeStaleExecution(message string) error {
	return &NativeError{Code: "stale_execution", Message: message, Status: http.StatusConflict}
}

func NativeRunEventStorageUnavailable() error {
	return &NativeError{Code: "not_ready", Message: "Run event storage is not available", Status: http.StatusServiceUnavailable, PublicMessage: true}
}
