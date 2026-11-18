package templates_test

import (
	"os"
	"strings"
	"testing"
)

func TestGlobalCSSStylesThinScrollbars(t *testing.T) {
	t.Parallel()

	css, err := os.ReadFile("../../../static/css/input.css")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(css)

	for _, want := range []string{
		"::-webkit-scrollbar",
		"::-webkit-scrollbar-thumb",
		"scrollbar-color: var(--color-line) transparent;",
		"scrollbar-width: thin;",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("global CSS missing %q", want)
		}
	}
}
