package cli

import (
	"io"
	"strings"
)

var terminalDashboardSuppressedSequences = []string{
	"\x1b[?25l",
	"\x1b[?25h",
	"\x1b[?1002h",
	"\x1b[?1002l",
	"\x1b[?1003h",
	"\x1b[?1003l",
	"\x1b[?1006h",
	"\x1b[?1006l",
	"\x1b[?1049h",
	"\x1b[?1049l",
	"\x1b[?2004h",
	"\x1b[?2004l",
	"\x1b[2K",
}

type terminalDashboardOutputFilter struct {
	out     io.Writer
	pending string
}

func newTerminalDashboardOutputFilter(out io.Writer) io.Writer {
	if out == nil {
		return io.Discard
	}
	return &terminalDashboardOutputFilter{out: out}
}

func (w *terminalDashboardOutputFilter) Write(p []byte) (int, error) {
	w.pending += string(p)
	filtered, pending := filterTerminalDashboardOutput(w.pending)
	w.pending = pending
	if filtered == "" {
		return len(p), nil
	}
	if _, err := io.WriteString(w.out, filtered); err != nil {
		return 0, err
	}
	return len(p), nil
}

func filterTerminalDashboardOutput(input string) (string, string) {
	var out strings.Builder
	for len(input) > 0 {
		if matched, rest := consumeSuppressedTerminalSequence(input); matched {
			input = rest
			continue
		}
		if partialSuppressedTerminalSequence(input) {
			return out.String(), input
		}
		out.WriteByte(input[0])
		input = input[1:]
	}
	return out.String(), ""
}

func consumeSuppressedTerminalSequence(input string) (bool, string) {
	for _, seq := range terminalDashboardSuppressedSequences {
		if strings.HasPrefix(input, seq) {
			return true, input[len(seq):]
		}
	}
	return false, input
}

func partialSuppressedTerminalSequence(input string) bool {
	for _, seq := range terminalDashboardSuppressedSequences {
		if len(input) < len(seq) && strings.HasPrefix(seq, input) {
			return true
		}
	}
	return false
}
