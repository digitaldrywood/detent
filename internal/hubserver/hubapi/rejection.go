package hubapi

import (
	"encoding/hex"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func Rejection(err error, category, field string) error {
	var failure *Error
	rejection := runnerauth.PublicationRejection{Category: category, Field: field}
	if !errors.As(err, &failure) || failure.Code != "invalid_request" || !rejection.Valid() {
		return err
	}
	result := &Error{Code: failure.Code, Message: rejection.DiagnosticError(), Status: failure.Status, Details: map[string]any{"rejection": rejection}}
	switch category {
	case "event_validation", "runtime_validation", "execution_validation", "usage_validation", "owner_validation", "body_decode":
		result.Message, result.Public = failure.Message, true
	}
	return result
}

const SourceRefusalGuidance = "Source bundle metadata or pack is invalid or exceeds the verification budget; split large changes or recreate it with git bundle create <path> HEAD ^<base>"

func OperatorSourceRefusal(err error) (*operatortool.RequestError, bool) {
	var failure *NativeError
	if !errors.As(FromFeatureError(err), &failure) || failure.Code != "invalid_request" {
		return nil, false
	}
	rejection, ok := failure.Details["rejection"].(runnerauth.PublicationRejection)
	if !ok || rejection.Field != "source" || rejection.Category != "version_validation" && rejection.Category != "source_verification" {
		return nil, false
	}
	return &operatortool.RequestError{Code: failure.Code, Message: SourceRefusalGuidance}, true
}

func ValidID(id, prefix string) bool {
	value, ok := strings.CutPrefix(id, prefix+"_")
	length := 32
	if prefix == "policy" {
		length = 64
	}
	if !ok || len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
