package operatortool

import "github.com/digitaldrywood/detent/internal/tracker"

type RequestError struct {
	Code    string                     `json:"code"`
	Message string                     `json:"message"`
	Archive *tracker.ArchiveDescriptor `json:"archive,omitempty"`
}

func (e *RequestError) Error() string { return e.Code }

func (e *RequestError) Unwrap() error { return ErrInvalidArguments }
