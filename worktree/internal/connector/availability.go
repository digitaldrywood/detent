package connector

import (
	"errors"
	"strings"
)

const (
	TrackerUnavailableCondition       = "tracker_unavailable"
	TrackerAvailabilityClassServer    = "server"
	TrackerAvailabilityClassTimeout   = "timeout"
	TrackerAvailabilityClassTransport = "transport"
)

var ErrTrackerUnavailable = errors.New("tracker unavailable")

type TrackerAvailabilityScope struct {
	Connector          string `json:"connector"`
	Endpoint           string `json:"endpoint"`
	Operation          string `json:"operation"`
	CredentialIdentity string `json:"credential_identity,omitempty"`
}

func (s TrackerAvailabilityScope) Normalize() TrackerAvailabilityScope {
	s.Connector = strings.ToLower(strings.TrimSpace(s.Connector))
	s.Endpoint = strings.TrimSpace(s.Endpoint)
	s.Operation = strings.ToLower(strings.TrimSpace(s.Operation))
	s.CredentialIdentity = strings.TrimSpace(s.CredentialIdentity)
	return s
}

func (s TrackerAvailabilityScope) Key(class string) string {
	s = s.Normalize()
	return strings.Join([]string{
		s.Connector,
		s.Endpoint,
		s.Operation,
		strings.ToLower(strings.TrimSpace(class)),
		s.CredentialIdentity,
	}, "\x00")
}

type TrackerAvailabilityError struct {
	Scope TrackerAvailabilityScope
	Class string
	Err   error
}

func NewTrackerAvailabilityError(scope TrackerAvailabilityScope, class string, err error) *TrackerAvailabilityError {
	return &TrackerAvailabilityError{
		Scope: scope.Normalize(),
		Class: strings.ToLower(strings.TrimSpace(class)),
		Err:   err,
	}
}

func (e *TrackerAvailabilityError) Error() string {
	if e == nil {
		return ErrTrackerUnavailable.Error()
	}
	tracker := strings.TrimSpace(e.Scope.Connector)
	if tracker == "" {
		tracker = "configured"
	}
	message := "tracker " + tracker + " unavailable (" + TrackerUnavailableCondition
	if class := strings.TrimSpace(e.Class); class != "" {
		message += "/" + class
	}
	message += ")"
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

func (e *TrackerAvailabilityError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *TrackerAvailabilityError) Is(target error) bool {
	return target == ErrTrackerUnavailable
}

func AsTrackerAvailability(err error) (*TrackerAvailabilityError, bool) {
	var availabilityErr *TrackerAvailabilityError
	if !errors.As(err, &availabilityErr) || availabilityErr == nil {
		return nil, false
	}
	return availabilityErr, true
}
