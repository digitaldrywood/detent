package linear

import (
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

var (
	ErrCommentCreateFailed = errors.New("linear comment create failed")
	ErrGraphQLErrors       = errors.New("linear graphql errors")
	ErrInvalidEndpoint     = errors.New("linear endpoint is invalid")
	ErrInvalidResponse     = errors.New("linear response is invalid")
	ErrIssueNotFound       = errors.New("linear issue not found")
	ErrIssueUpdateFailed   = errors.New("linear issue update failed")
	ErrMissingIssue        = errors.New("linear issue is required")
	ErrMissingUser         = errors.New("linear user is required")
	ErrMissingToken        = errors.New("linear token is required")
	ErrStateNotFound       = errors.New("linear state not found")
	ErrTransient           = connector.NewRetryableError("linear transient error")
	ErrUnexpectedStatus    = errors.New("linear unexpected status")
	ErrUserAmbiguous       = errors.New("linear user ambiguous")
	ErrUserNotFound        = errors.New("linear user not found")
)

type GraphQLError struct {
	Message string `json:"message"`
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
