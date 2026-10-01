package operatortool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Catch bounds or authority fields reaching the shared application/preview path.
func TestProjectArgumentBounds(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		target    func() any
		invalid   bool
	}{
		{"bounded read", `{"project_id":"p","limit":200}`, func() any { return &ProjectReadRequest{} }, false},
		{"large page", `{"limit":201}`, func() any { return &ProjectReadRequest{} }, true},
		{"negative page", `{"limit":-1}`, func() any { return &ProjectReadRequest{} }, true},
		{"long project", `{"project_id":"` + strings.Repeat("p", 257) + `"}`, func() any { return &ProjectReadRequest{} }, true},
		{"nested secret", `{"input":{"token":"secret"}}`, func() any { return &ProjectRequest[struct{}]{} }, true},
		{"forged authority", `{"input":{},"yolo":true}`, func() any { return &ProjectRequest[struct{}]{} }, true},
		{"null input", `{"input":null}`, func() any { return &ProjectRequest[struct{}]{} }, true},
		{"negative issue", `{"input":{"issue_number":-1}}`, func() any { return &ProjectRequest[ImportStartInput]{} }, true},
		{"too many states", `{"input":{"states":[` + strings.TrimSuffix(strings.Repeat(`{},`, 201), ",") + `]}}`, func() any { return &ProjectRequest[ProjectCreateInput]{} }, true},
		{"too many intake numbers", `{"input":{"numbers":[` + strings.TrimSuffix(strings.Repeat(`1,`, 201), ",") + `]}}`, func() any { return &ProjectRequest[GitHubBatchInput]{} }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := DecodeProjectArguments(json.RawMessage(tt.raw), tt.target())
			if (err != nil) != tt.invalid || err != nil && !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("decode=%v want invalid=%v", err, tt.invalid)
			}
		})
	}
}
