package cli

import (
	"bytes"
	"testing"
)

func TestTerminalDashboardOutputFilterSuppressesModeSequences(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer := newTerminalDashboardOutputFilter(&output)
	input := "\x1b[?25lDetent\x1b[2K\r\x1b[?2004l\x1b[?25h\x1b[?1002l\x1b[?1003l\x1b[?1006l"

	if _, err := writer.Write([]byte(input)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := output.String(); got != "Detent\r" {
		t.Fatalf("filtered output = %q, want %q", got, "Detent\r")
	}
}

func TestTerminalDashboardOutputFilterHandlesSplitSequences(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writer := newTerminalDashboardOutputFilter(&output)
	for _, chunk := range []string{"ready", "\x1b[?", "25", "h", "\n"} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q) error = %v", chunk, err)
		}
	}

	if got := output.String(); got != "ready\n" {
		t.Fatalf("filtered split output = %q, want %q", got, "ready\n")
	}
}
