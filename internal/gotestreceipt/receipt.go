package gotestreceipt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	WrapperName         = "detent-go-test-exec"
	SocketEnvironment   = "DETENT_GO_TEST_RECEIPTS"
	RootEnvironment     = "DETENT_GO_TEST_ROOT"
	MaxInputs           = 4096
	MaxCandidates       = 8
	maxInputLength      = 1024
	maxInputBytes       = 96 << 10
	maxPackageLength    = 512
	maxAttestation      = 128
	DigestKeyBytes      = 32
	protocolFingerprint = "detent-go-test-receipt-v5"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Candidate struct {
	Inputs []string `json:"inputs"`
	Digest string   `json:"digest"`
}

type Attestation struct {
	ID        string `json:"id"`
	DigestKey []byte `json:"digest_key"`
}

type Receipt struct {
	Attestation string   `json:"attestation"`
	Key         string   `json:"key"`
	Package     string   `json:"package"`
	Inputs      []string `json:"inputs"`
	Digest      string   `json:"digest"`
}

type Store interface {
	Attest(ctx context.Context) (Attestation, error)
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

func (a Attestation) Validate() error {
	if a.ID == "" || len(a.ID) > maxAttestation || strings.ContainsAny(a.ID, "\t\r\n\x00") || len(a.DigestKey) != DigestKeyBytes {
		return errors.New("receipt attestation is invalid")
	}
	return nil
}

func (r Receipt) Validate() error {
	if r.Attestation == "" || len(r.Attestation) > maxAttestation || strings.ContainsAny(r.Attestation, "\t\r\n\x00") {
		return errors.New("receipt attestation is required")
	}
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
		fields := strings.Split(input, "\t")
		switch {
		case len(fields) == 1 && fields[0] == environInput:
		case len(fields) == 2 && fields[0] == "env" && fields[1] != "" && !strings.ContainsAny(fields[1], "=/\\:"):
		case len(fields) == 3 && (fields[0] == "open" || fields[0] == "stat" || fields[0] == treeInput) && validPathScope(fields[1]) && validRelativePath(fields[2]):
		default:
			return errors.New("receipt input must name an environment variable or checkout-relative path")
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

var tempRootTokens = []string{"system", "TMPDIR", "TMP", "TEMP", "GOTMPDIR", "tmp", "vartmp"}

func validPathScope(scope string) bool {
	if token, ok := strings.CutPrefix(scope, scopeTemporary); ok {
		return slices.Contains(tempRootTokens, token)
	}
	return scope == scopeRoot || scope == scopeGoRoot
}

func validRelativePath(name string) bool {
	return name != "" && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\:")
}
