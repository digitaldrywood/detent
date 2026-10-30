package gotestreceipt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	WrapperName         = "detent-go-test-exec"
	SocketEnvironment   = "DETENT_GO_TEST_RECEIPTS"
	RootEnvironment     = "DETENT_GO_TEST_ROOT"
	TokenEnvironment    = "DETENT_GO_TEST_RECEIPTS_TOKEN"
	MaxInputs           = 4096
	MaxCandidates       = 8
	maxInputLength      = 1024
	maxInputBytes       = 96 << 10
	maxPackageLength    = 512
	protocolFingerprint = "detent-go-test-receipt-v2"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Candidate struct {
	Inputs []string `json:"inputs"`
	Digest string   `json:"digest"`
}

type Receipt struct {
	Key     string   `json:"key"`
	Package string   `json:"package"`
	Inputs  []string `json:"inputs"`
	Digest  string   `json:"digest"`
}

type Store interface {
	Lookup(ctx context.Context, key string) ([]Candidate, error)
	Record(ctx context.Context, receipt Receipt) error
}

type Config struct {
	Store   Store
	Wrapper string
}

type contextKey struct{}

func WithContext(ctx context.Context, config Config) context.Context {
	if config.Store == nil || config.Wrapper == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, config)
}

func FromContext(ctx context.Context) (Config, bool) {
	config, ok := ctx.Value(contextKey{}).(Config)
	return config, ok
}

func ValidKey(key string) bool {
	return digestPattern.MatchString(key)
}

func (r Receipt) Validate() error {
	if !ValidKey(r.Key) || !ValidKey(r.Digest) {
		return errors.New("receipt key and digest must be sha256 hex")
	}
	if len(r.Package) > maxPackageLength || strings.ContainsAny(r.Package, "\r\n") {
		return errors.New("receipt package is invalid")
	}
	return ValidateInputs(r.Inputs)
}

func ValidateInputs(inputs []string) error {
	if len(inputs) > MaxInputs {
		return fmt.Errorf("receipt has %d inputs, limit %d", len(inputs), MaxInputs)
	}
	for _, input := range inputs {
		if input == "" || len(input) > maxInputLength || strings.ContainsAny(input, "\r\n\x00") {
			return errors.New("receipt input is invalid")
		}
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return fmt.Errorf("encode receipt inputs: %w", err)
	}
	if len(encoded) > maxInputBytes {
		return fmt.Errorf("receipt inputs exceed %d encoded bytes", maxInputBytes)
	}
	return nil
}
