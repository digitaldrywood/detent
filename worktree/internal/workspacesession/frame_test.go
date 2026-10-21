package workspacesession_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestValidChannel(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"terminal", "files", "diff", "preview"} {
		if !workspacesession.ValidChannel(name) {
			t.Errorf("ValidChannel(%q) = false, want true", name)
		}
		// A channel and the capability it needs share a vocabulary on
		// purpose: a surface enables from what the runner reported, and
		// nothing else.
		if !workspacesession.ValidCapability(workspacesession.ChannelCapability(name)) {
			t.Errorf("ChannelCapability(%q) names no capability", name)
		}
	}
	for _, name := range []string{"agents", "", "Files"} {
		if workspacesession.ValidChannel(name) {
			t.Errorf("ValidChannel(%q) = true, want false", name)
		}
	}
}

func TestStreamID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		value      string
		connection string
		n          int
		wantErr    bool
	}{
		{name: "allocated", value: workspacesession.StreamID("conn_1", 3), connection: "conn_1", n: 3},
		// The connection half may itself contain a colon, so the split is
		// from the right: a stream belongs to the connection that opened it,
		// and mis-parsing that would hand one person another's frames.
		{name: "colon in the connection", value: "a:b:7", connection: "a:b", n: 7},
		{name: "no counter", value: "conn_1:", wantErr: true},
		{name: "no connection", value: ":1", wantErr: true},
		{name: "no separator", value: "conn_1", wantErr: true},
		{name: "zero counter", value: "conn_1:0", wantErr: true},
		{name: "negative counter", value: "conn_1:-1", wantErr: true},
		{name: "not a number", value: "conn_1:x", wantErr: true},
		{name: "empty", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			connection, n, err := workspacesession.ParseStreamID(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseStreamID(%q) = %q, %d, want an error", test.value, connection, n)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseStreamID(%q) = %v", test.value, err)
			}
			if connection != test.connection || n != test.n {
				t.Fatalf("ParseStreamID(%q) = %q, %d, want %q, %d", test.value, connection, n, test.connection, test.n)
			}
		})
	}
}

func TestValidateFrame(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		frame   workspacesession.Frame
		wantErr bool
	}{
		{name: "a files request", frame: workspacesession.Frame{Channel: "files", Type: "list", Stream: "conn:1", Seq: 1}},
		{name: "a control with no stream", frame: workspacesession.Frame{Channel: "files", Type: "ack"}},
		{name: "unknown channel", frame: workspacesession.Frame{Channel: "agents", Type: "list"}, wantErr: true},
		{name: "no type", frame: workspacesession.Frame{Channel: "files"}, wantErr: true},
		{name: "blank type", frame: workspacesession.Frame{Channel: "files", Type: "  "}, wantErr: true},
		{name: "negative sequence", frame: workspacesession.Frame{Channel: "files", Type: "list", Seq: -1}, wantErr: true},
		{name: "unparseable stream", frame: workspacesession.Frame{Channel: "files", Type: "list", Stream: "nope"}, wantErr: true},
		{
			name: "over the frame cap",
			frame: workspacesession.Frame{
				Channel: "files", Type: "content",
				Payload: json.RawMessage(strings.Repeat("a", workspacesession.MaxFrameBytes+1)),
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := workspacesession.ValidateFrame(test.frame)
			if test.wantErr && err == nil {
				t.Fatalf("ValidateFrame(%+v) = nil, want an error", test.frame)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("ValidateFrame(%+v) = %v", test.frame, err)
			}
		})
	}
}

func TestErrorFrame(t *testing.T) {
	t.Parallel()
	frame := workspacesession.ErrorFrame("files", "conn:1", workspacesession.CodeDenied, "the project's secret patterns refuse this path")
	if frame.Type != workspacesession.TypeError || frame.Channel != "files" || frame.Stream != "conn:1" {
		t.Fatalf("ErrorFrame = %+v", frame)
	}
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeDenied || payload.Message == "" {
		t.Fatalf("payload = %+v", payload)
	}
}

// TestLimitsMatchTheContract pins the numbers section 18.2 and 18.4 state. They
// are constants rather than configuration precisely so a client and a runner
// cannot disagree about when a stream dies, which makes a drift here a
// protocol change rather than a tuning change.
func TestLimitsMatchTheContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  int64
		want int64
	}{
		{name: "frame cap", got: workspacesession.MaxFrameBytes, want: 256 << 10},
		{name: "streams per connection", got: workspacesession.MaxStreamsPerConnection, want: 8},
		{name: "streams per workspace", got: workspacesession.MaxStreamsPerWorkspace, want: 32},
		{name: "stream buffer", got: workspacesession.StreamBufferBytes, want: 1 << 20},
		{name: "ack every", got: workspacesession.AckEvery, want: 32},
		{name: "relay memory", got: workspacesession.DefaultRelayMemoryBytes, want: 256 << 20},
		{name: "ticket bytes", got: workspacesession.TicketBytes, want: 32},
		{name: "read cap", got: workspacesession.MaxReadBytes, want: 2 << 20},
		{name: "directory page", got: workspacesession.DirectoryPage, want: 500},
		{name: "reads in flight", got: workspacesession.MaxReadsInFlight, want: 4},
		{name: "resume window seconds", got: int64(workspacesession.ResumeWindow.Seconds()), want: 60},
		{name: "ticket lifetime seconds", got: int64(workspacesession.TicketLifetime.Seconds()), want: 30},
		{name: "heartbeat seconds", got: int64(workspacesession.HeartbeatInterval.Seconds()), want: 30},
		{name: "missed heartbeats", got: workspacesession.MissedHeartbeats, want: 2},
		{name: "lease ttl seconds", got: int64(workspacesession.LeaseTTL.Seconds()), want: 90},
		{name: "rebind grace seconds", got: int64(workspacesession.RebindGrace.Seconds()), want: 30},
		{name: "authority recheck seconds", got: int64(workspacesession.AuthorityRecheckInterval.Seconds()), want: 30},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.got != test.want {
				t.Fatalf("%s = %d, want %d", test.name, test.got, test.want)
			}
		})
	}
}
