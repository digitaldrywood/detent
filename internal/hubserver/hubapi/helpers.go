package hubapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

const DefaultCleanupBudget = 5 * time.Second

const MaxRequestBodyBytes int64 = 1 << 20

func NewID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func CommandHash(input any) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func DecodeJSON(c echo.Context, target any) error {
	return DecodeJSONLimit(c, target, MaxRequestBodyBytes)
}

func DecodeJSONLimit(c echo.Context, target any, limit int64) error {
	if c == nil || c.Request() == nil {
		return errors.New("request is required")
	}
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func MarshalString(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func ParseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
}
