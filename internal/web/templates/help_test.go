package templates

import (
	"os"
	"strings"
	"testing"
)

func TestHelpCSSKeepsPanelsPointerTransparent(t *testing.T) {
	t.Parallel()

	css, err := os.ReadFile("../../../static/css/input.css")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(css)

	for _, want := range []string{
		".popover-panel",
		"pointer-events: none;",
		`.help-tooltip[data-open="true"]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("CSS missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, ".help-tip:hover .help-tip-panel") {
		t.Fatalf("CSS hover-open rule can fight JS placement:\n%s", text)
	}
}
