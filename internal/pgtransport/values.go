package pgtransport

import (
	"encoding/json"
	"errors"
	"time"
)

type State string

const (
	Serialization State = "40001"
	Deadlock      State = "40P01"
	Unique        State = "23505"
	ForeignKey    State = "23503"
	Check         State = "23514"
	LockTimeout   State = "55P03"
	Cancel        State = "57014"
)

func SQLState(err error) State {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		return State(state.SQLState())
	}
	return ""
}

func Timestamp(value time.Time) time.Time { return value.UTC().Truncate(time.Microsecond) }
func Nullable[T any](value T, valid bool) *T {
	if !valid {
		return nil
	}
	return &value
}
func Array[T any](value []T) []T {
	if value == nil {
		return []T{}
	}
	return value
}
func JSONB(value json.RawMessage) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	if !json.Valid(value) {
		return nil, errors.New("invalid PostgreSQL JSONB")
	}
	return value, nil
}
