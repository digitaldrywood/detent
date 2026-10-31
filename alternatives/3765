package workspacesession_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestGitRequestVocabulary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request string
		valid   bool
		write   bool
	}{
		{name: "status reads", request: "status", valid: true},
		{name: "commit writes", request: "commit", valid: true, write: true},
		{name: "push writes", request: "push", valid: true, write: true},
		{name: "a files frame is not a git frame", request: "list"},
		{name: "an answer is not a request", request: "committed"},
		{name: "an empty type names nothing", request: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := workspacesession.ValidGitRequest(test.request); got != test.valid {
				t.Fatalf("ValidGitRequest(%q) = %v, want %v", test.request, got, test.valid)
			}
			if got := workspacesession.GitWriteRequest(test.request); got != test.write {
				t.Fatalf("GitWriteRequest(%q) = %v, want %v", test.request, got, test.write)
			}
		})
	}
}

func TestValidateGitRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		requestType string
		message     string
		wantErr     bool
	}{
		{name: "a status needs no message", requestType: "status"},
		{name: "a push needs no message", requestType: "push"},
		{name: "a commit message is accepted", requestType: "commit", message: "fix(git): commit from the header"},
		{name: "a commit with no message is refused", requestType: "commit", wantErr: true},
		{name: "a commit with only spaces is refused", requestType: "commit", message: "   \n\t ", wantErr: true},
		{
			name:        "a commit message past the cap is refused",
			requestType: "commit",
			message:     strings.Repeat("m", workspacesession.MaxCommitMessageBytes+1),
			wantErr:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := workspacesession.ValidateGitRequest(test.requestType, workspacesession.GitRequest{Message: test.message})
			if test.wantErr {
				if err == nil {
					t.Fatalf("ValidateGitRequest(%q) = nil, want an error", test.requestType)
				}
				if !errors.Is(err, workspacesession.ErrInvalidFrame) {
					t.Fatalf("ValidateGitRequest(%q) = %v, want ErrInvalidFrame", test.requestType, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateGitRequest(%q) = %v", test.requestType, err)
			}
		})
	}
}

func TestGitChannelIsAChannel(t *testing.T) {
	t.Parallel()
	if !workspacesession.ValidChannel(workspacesession.ChannelGit) {
		t.Fatal("the git channel is not a channel")
	}
	if got := workspacesession.ChannelCapability(workspacesession.ChannelGit); got != workspacesession.CapabilityGit {
		t.Fatalf("ChannelCapability(git) = %q, want %q", got, workspacesession.CapabilityGit)
	}
	if err := workspacesession.ValidateFrame(workspacesession.Frame{
		Channel: workspacesession.ChannelGit, Type: workspacesession.TypeGitStatus,
	}); err != nil {
		t.Fatalf("ValidateFrame(git status) = %v", err)
	}
}

func TestGitFailedFrameCarriesStderr(t *testing.T) {
	t.Parallel()
	frame := workspacesession.GitFailedFrame(workspacesession.ChannelGit, "conn:1",
		"The push was refused", "! [rejected] main -> main (fetch first)")
	if frame.Type != workspacesession.TypeError {
		t.Fatalf("frame type = %q, want %q", frame.Type, workspacesession.TypeError)
	}
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Code != workspacesession.CodeGitFailed {
		t.Fatalf("code = %q, want %q", payload.Code, workspacesession.CodeGitFailed)
	}
	if !strings.Contains(payload.Stderr, "rejected") {
		t.Fatalf("stderr = %q, want git's own output", payload.Stderr)
	}
}
