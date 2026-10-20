package workspacesession

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestTerminalRequestTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "open", value: TypeTerminalOpen, want: true},
		{name: "input", value: TypeTerminalInput, want: true},
		{name: "resize", value: TypeTerminalResize, want: true},
		// The runner's answers are not requests: a person who sent one would be
		// impersonating the runner, and the channel answers unknown_frame.
		{name: "opened is the runner's", value: TypeTerminalOpened},
		{name: "output is the runner's", value: TypeTerminalOutput},
		{name: "exit is the runner's", value: TypeTerminalExit},
		// close is a shared control the relay answers for every channel, so it
		// is not in the terminal's own vocabulary.
		{name: "close is a shared control", value: TypeClose},
		{name: "an exec run is another channel's", value: TypeExecRun},
		{name: "empty", value: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ValidTerminalRequest(tt.value); got != tt.want {
				t.Fatalf("ValidTerminalRequest(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestTerminalChannelIsNamedAndMapped(t *testing.T) {
	t.Parallel()

	if !ValidChannel(ChannelTerminal) {
		t.Fatal("ValidChannel(terminal) = false, want true")
	}
	if !slices.Contains(ChannelNames(), ChannelTerminal) {
		t.Fatalf("ChannelNames() = %v, want it to carry the terminal", ChannelNames())
	}
	if got := ChannelCapability(ChannelTerminal); got != CapabilityTerminal {
		t.Fatalf("ChannelCapability(terminal) = %q, want %q", got, CapabilityTerminal)
	}
}

func TestNormalizeTerminalSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cols     int
		rows     int
		wantCols int
		wantRows int
		wantErr  bool
	}{
		{name: "an ordinary window is kept", cols: 120, rows: 40, wantCols: 120, wantRows: 40},
		{
			name:     "no size at all takes the conventional one",
			wantCols: DefaultTerminalCols,
			wantRows: DefaultTerminalRows,
		},
		{name: "the smallest legal window", cols: 1, rows: 1, wantCols: 1, wantRows: 1},
		{name: "the largest legal window", cols: MaxTerminalCols, rows: MaxTerminalRows, wantCols: MaxTerminalCols, wantRows: MaxTerminalRows},
		{name: "a zero width beside a height is refused", cols: 0, rows: 24, wantErr: true},
		{name: "a zero height beside a width is refused", cols: 80, rows: 0, wantErr: true},
		{name: "a negative width is refused", cols: -1, rows: 24, wantErr: true},
		{name: "a width past the winsize field is refused", cols: MaxTerminalCols + 1, rows: 24, wantErr: true},
		{name: "a height past the winsize field is refused", cols: 80, rows: MaxTerminalRows + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cols, rows, err := NormalizeTerminalSize(tt.cols, tt.rows)
			if tt.wantErr {
				if err == nil {
					t.Fatal("NormalizeTerminalSize() error = nil, want a refusal")
				}
				if !errors.Is(err, ErrInvalidFrame) {
					t.Fatalf("NormalizeTerminalSize() error = %v, want an invalid frame", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeTerminalSize() error = %v, want nil", err)
			}
			if cols != tt.wantCols || rows != tt.wantRows {
				t.Fatalf("NormalizeTerminalSize() = %d x %d, want %d x %d", cols, rows, tt.wantCols, tt.wantRows)
			}
		})
	}
}

func TestValidateTerminalOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request TerminalOpen
		want    TerminalOpen
		wantErr bool
	}{
		{
			name:    "a measured window opens as it stands",
			request: TerminalOpen{Cols: 100, Rows: 30},
			want:    TerminalOpen{Cols: 100, Rows: 30},
		},
		{
			name:    "an unmeasured window takes the defaults",
			request: TerminalOpen{},
			want:    TerminalOpen{Cols: DefaultTerminalCols, Rows: DefaultTerminalRows},
		},
		{
			name:    "a cwd is trimmed",
			request: TerminalOpen{Cols: 80, Rows: 24, Cwd: "  internal/app  "},
			want:    TerminalOpen{Cols: 80, Rows: 24, Cwd: "internal/app"},
		},
		{name: "a NUL in the cwd is refused", request: TerminalOpen{Cols: 80, Rows: 24, Cwd: "a\x00b"}, wantErr: true},
		{
			name:    "an oversized cwd is refused",
			request: TerminalOpen{Cols: 80, Rows: 24, Cwd: strings.Repeat("a", MaxTerminalCwdBytes+1)},
			wantErr: true,
		},
		{name: "an illegal window is refused", request: TerminalOpen{Cols: 0, Rows: 24}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ValidateTerminalOpen(tt.request)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ValidateTerminalOpen() error = nil, want a refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateTerminalOpen() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("ValidateTerminalOpen() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateTerminalResize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request TerminalResize
		want    TerminalResize
		wantErr bool
	}{
		{name: "a measured resize", request: TerminalResize{Cols: 132, Rows: 43}, want: TerminalResize{Cols: 132, Rows: 43}},
		// An open with no size is a client that has not measured yet and gets
		// the defaults; a resize with no size is a client sending nothing, and
		// defaulting it would silently shrink a window somebody was using.
		{name: "a resize naming nothing is refused", request: TerminalResize{}, wantErr: true},
		{name: "a half-named resize is refused", request: TerminalResize{Cols: 80}, wantErr: true},
		{name: "an oversized resize is refused", request: TerminalResize{Cols: MaxTerminalCols + 1, Rows: 24}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ValidateTerminalResize(tt.request)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ValidateTerminalResize() error = nil, want a refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateTerminalResize() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("ValidateTerminalResize() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateTerminalInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request TerminalInput
		wantErr bool
	}{
		{name: "a keystroke", request: TerminalInput{Data: "l"}},
		{name: "a control character", request: TerminalInput{Data: "\x03"}},
		{name: "an empty input is legal", request: TerminalInput{}},
		{name: "base64 is legal", request: TerminalInput{Data: "aGk=", Encoding: EncodingBase64}},
		{name: "an unknown encoding is refused", request: TerminalInput{Data: "hi", Encoding: "hex"}, wantErr: true},
		{
			name:    "an oversized paste is refused",
			request: TerminalInput{Data: strings.Repeat("a", MaxTerminalInputBytes+1)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateTerminalInput(tt.request)
			if tt.wantErr && err == nil {
				t.Fatal("ValidateTerminalInput() error = nil, want a refusal")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateTerminalInput() error = %v, want nil", err)
			}
		})
	}
}

func TestTerminalOutputFitsOneFrame(t *testing.T) {
	t.Parallel()

	// A terminal output span is always base64, which costs four bytes for every
	// three. The bound exists so an output frame never needs chunking on the
	// hot path, and this is the arithmetic that says so.
	encoded := (MaxTerminalOutputFrameBytes + 2) / 3 * 4
	if encoded >= MaxFrameBytes {
		t.Fatalf("a full output span encodes to %d bytes, which does not fit the %d byte frame cap", encoded, MaxFrameBytes)
	}
	if MaxTerminalInputBytes >= MaxFrameBytes {
		t.Fatalf("an input frame of %d bytes does not fit the %d byte frame cap", MaxTerminalInputBytes, MaxFrameBytes)
	}
}
