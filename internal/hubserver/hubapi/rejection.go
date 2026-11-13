package hubapi

import (
	"encoding/hex"
	"errors"
	"strings"

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
