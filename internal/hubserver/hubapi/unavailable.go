package hubapi

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func AttachmentAccountingUnavailable() error {
	return &NativeError{Code: "storage_owner_unavailable", Message: "Attachment accounting is unavailable with PostgreSQL storage", Status: http.StatusServiceUnavailable, PublicMessage: true}
}

var ErrPostgresFeatureUnavailable = errors.New("configured Hub feature is unavailable with PostgreSQL storage")

var ErrProjectServiceUnavailable = errors.New("project operation is unavailable")

func SafeWorkReadError(err error) error {
	var export *operatortool.WorkBulkExportItemError
	if errors.As(err, &export) {
		return err
	}
	var ambiguous *explain.AmbiguousIdentityError
	if errors.Is(err, explain.ErrNotFound) || errors.Is(err, operatortool.ErrProjectScopeRequired) || errors.Is(err, operatortool.ErrAccessDenied) || errors.Is(err, operatortool.ErrInvalidArguments) || errors.As(err, &ambiguous) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) || IsNativeNotFound(err) {
		return explain.ErrNotFound
	}
	if archive := NativeArchiveReadError(err); archive != nil {
		return archive
	}
	var native *NativeError
	if errors.As(err, &native) && native.Status == 422 {
		return &operatortool.RequestError{Code: native.Code, Message: native.Message}
	}
	return &operatortool.ReadUnavailableError{Err: err}
}

func PolicyMismatch(message string) error {
	return &NativeError{Code: "policy_mismatch", Message: message, Status: http.StatusConflict}
}

var ErrProjectParticipants = errors.New("project lifecycle participants are unavailable")

func ProjectRecordError(err error) error {
	if errors.Is(err, corestore.ErrInvalidRelationship) {
		return NativePublicInvalid("Relationship type, operation, or endpoints are invalid")
	}
	if errors.Is(err, corestore.ErrDependencyCycle) {
		return NativePublicInvalid("Dependencies cannot form a cycle")
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return NativeNotFound()
	}
	if errors.Is(err, ErrProjectParticipants) {
		return &NativeError{Code: "not_ready", Message: "Project lifecycle storage is unavailable", Status: http.StatusServiceUnavailable, PublicMessage: true}
	}
	if errors.Is(err, projectstore.ErrStateInUse) {
		return NativeWorkflowInvalid("Workflow states used by work items cannot be removed")
	}
	if errors.Is(err, projectstore.ErrActiveWork) {
		return NativePublicInvalid("Finish or cancel active work before deleting this project")
	}
	return err
}

func NativeWorkItemStorageUnavailable() error {
	return FromFeatureError(WorkItemStorageUnavailable())
}

// WorkspaceUnavailable reports that the hub is not running workspace sessions.
func WorkspaceUnavailable() error {
	return &NativeError{
		Code: "unsupported_control", Message: "Workspace sessions are not enabled on this hub",
		Status:        http.StatusUnprocessableEntity,
		PublicMessage: true,
	}
}
