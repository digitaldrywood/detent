package workspacerunner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestTerminalStreamHoldsOutputWhileTheSocketIsDown(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", pendingLimit/2+1)
	tests := []struct {
		name      string
		spans     []string
		down      bool
		wantSent  []string
		wantHeld  []string
		cancelled bool
		wantErr   bool
	}{
		{name: "a live socket sends in order", spans: []string{"a", "b"}, wantSent: []string{"a", "b"}},
		{name: "a dropped socket holds without failing the reader", spans: []string{"a", "b"}, down: true, wantHeld: []string{"a", "b"}},
		{name: "held output is bounded by dropping the oldest", spans: []string{"a", big, big}, down: true, wantHeld: []string{big}},
		{name: "a finished session stops the reader", spans: []string{"a"}, down: true, cancelled: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancelled {
				cancel()
			}
			var sent []string
			send := func(span workspacesession.TerminalOutput) error {
				if test.down {
					return errNoRelaySocket
				}
				sent = append(sent, span.Data)
				return nil
			}
			held := &terminalStream{}
			var err error
			for _, data := range test.spans {
				err = errors.Join(err, held.deliver(ctx, send, workspacesession.TerminalOutput{Data: data}))
			}
			if (err != nil) != test.wantErr {
				t.Fatalf("deliver() error = %v, want error %t", err, test.wantErr)
			}
			if !slices.Equal(sent, test.wantSent) {
				t.Fatalf("sent %d spans, want %d", len(sent), len(test.wantSent))
			}
			var pending []string
			for _, span := range held.pending {
				pending = append(pending, span.Data)
			}
			if !test.cancelled && !slices.Equal(pending, test.wantHeld) {
				t.Fatalf("held %d spans, want %d", len(pending), len(test.wantHeld))
			}
			if !test.down {
				return
			}
			test.down = false
			if err := held.flush(send); err != nil {
				t.Fatal(err)
			}
			if !test.cancelled && !slices.Equal(sent, test.wantHeld) {
				t.Fatalf("flushed %d spans, want %d in order", len(sent), len(test.wantHeld))
			}
			if len(held.pending) != 0 || held.pendingBytes != 0 {
				t.Fatalf("pending = %d spans, %d bytes after a flush", len(held.pending), held.pendingBytes)
			}
		})
	}
}
