package pgtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"
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

func Classify(err error) State {
	if state := SQLState(err); state != "" {
		return state
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Cancel
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
func Array[T any](value []T) []T { return value }
func JSONB(value json.RawMessage) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	if !json.Valid(value) || !utf8.Valid(value) || !validJSONUnicode(value) {
		return nil, errors.New("invalid PostgreSQL JSONB")
	}
	return value, nil
}

func validJSONUnicode(value []byte) bool {
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			continue
		}
		index++
		if index >= len(value) || value[index] != 'u' {
			continue
		}
		scalar, err := strconv.ParseUint(string(value[index+1:index+5]), 16, 16)
		if err != nil || scalar == 0 || scalar >= 0xDC00 && scalar <= 0xDFFF {
			return false
		}
		if scalar >= 0xD800 && scalar <= 0xDBFF {
			if index+11 > len(value) || !bytes.Equal(value[index+5:index+7], []byte(`\u`)) {
				return false
			}
			low, err := strconv.ParseUint(string(value[index+7:index+11]), 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			index += 6
		}
		index += 4
	}
	return true
}
