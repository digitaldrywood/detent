package templates

import (
	"bytes"
	"context"
	"testing"

	"github.com/a-h/templ"
)

func renderSSEFingerprintComponent(t *testing.T, component templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := component.Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}
