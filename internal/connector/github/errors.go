package github

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrAuthenticationFailed       = errors.New("github authentication failed")
	ErrGraphQLErrors              = errors.New("github graphql errors")
	ErrInvalidEndpoint            = errors.New("github endpoint is invalid")
	ErrInvalidPrivateKey          = errors.New("github app private key is invalid")
	ErrInvalidResponse            = errors.New("github response is invalid")
	ErrMissingAppConfig           = errors.New("github app configuration is incomplete")
	ErrMissingProject             = errors.New("github project is required")
	ErrMissingRepository          = errors.New("github repository is required")
	ErrMissingToken               = errors.New("github token is required")
	ErrNotFound                   = errors.New("github resource not found")
	ErrAssigneeNotFound           = errors.New("github assignee not found")
	ErrAssigneeUpdateFailed       = errors.New("github assignee update failed")
	ErrCommentCreateFailed        = errors.New("github comment create failed")
	ErrIssueFieldUpdateFailed     = errors.New("github issue field update failed")
	ErrIssueCloseFailed           = errors.New("github issue close failed")
	ErrProjectItemNotFound        = errors.New("github project item not found")
	ErrProjectFieldNotFound       = errors.New("github project field not found")
	ErrProjectFieldOptionNotFound = errors.New("github project field option not found")
	ErrProjectFieldUpdateFailed   = errors.New("github project field update failed")
	ErrProjectNotFound            = errors.New("github project not found")
	ErrRateLimited                = errors.New("github rate limited")
	ErrRESTBudgetReserved         = errors.New("github rest budget reserved")
	ErrStatusFieldNotFound        = errors.New("github status field not found")
	ErrStatusOptionNotFound       = errors.New("github status option not found")
	ErrStatusUpdateFailed         = errors.New("github status update failed")
	ErrTransient                  = errors.New("github transient error")
	ErrUnexpectedStatus           = errors.New("github unexpected status")
)

type GraphQLError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type GraphQLErrorList struct {
	Errors []GraphQLError
	Err    error
}

func (e *GraphQLErrorList) Error() string {
	if len(e.Errors) == 0 {
		return e.Err.Error()
	}

	messages := make([]string, 0, len(e.Errors))
	for _, err := range e.Errors {
		if strings.TrimSpace(err.Message) != "" {
			messages = append(messages, err.Message)
		}
	}
	if len(messages) == 0 {
		return e.Err.Error()
	}

	return fmt.Sprintf("%s: %s", e.Err, strings.Join(messages, "; "))
}

func (e *GraphQLErrorList) Unwrap() error {
	return e.Err
}

type StatusError struct {
	StatusCode int
	Body       string
	Err        error
}

func (e *StatusError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("%s: status %d", e.Err, e.StatusCode)
	}
	return fmt.Sprintf("%s: status %d: %s", e.Err, e.StatusCode, e.Body)
}

func (e *StatusError) Unwrap() error {
	return e.Err
}
