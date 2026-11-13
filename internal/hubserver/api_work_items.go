package hubserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const (
	defaultAPIPageLimit = 50
	maxAPIPageLimit     = 200
)

type timelineCursor struct {
	Version int   `json:"v"`
	ID      int64 `json:"id"`
}

func parsePageLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultAPIPageLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxAPIPageLimit {
		return 0, errors.New("page limit is invalid")
	}
	return limit, nil
}

func encodeTimelineCursor(cursor timelineCursor) (string, error) {
	value, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func decodeTimelineCursor(value string) (timelineCursor, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return timelineCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return timelineCursor{}, err
	}
	var cursor timelineCursor
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Version != 1 || cursor.ID <= 0 {
		return timelineCursor{}, errors.New("invalid timeline cursor")
	}
	return cursor, nil
}
