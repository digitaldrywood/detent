package api

import (
	"errors"
)

func HubErrorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		return apiErr.Code
	}
	return ""
}

// NativeLeaseLost reports whether the hub refused a mutation because the lease
// that fenced it is no longer the current one.
func NativeLeaseLost(err error) bool {
	if ClaimLost(err) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr != nil && apiErr.Code == "stale_execution"
}
