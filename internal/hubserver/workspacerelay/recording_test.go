package workspacerelay

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestTerminalRecordingAudience(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		isolation string
		role      string
		subject   string
		want      bool
	}{
		{
			name:      "the person who ran it reads their own at any level",
			isolation: workspacesession.IsolationUser,
			role:      "member",
			subject:   "user_ran_it",
			want:      true,
		},
		{name: "an owner reads a container recording", isolation: workspacesession.IsolationContainer, role: "owner", want: true},
		{name: "an admin reads a container recording", isolation: workspacesession.IsolationContainer, role: "admin", want: true},
		{name: "a member does not read somebody else's", isolation: workspacesession.IsolationContainer, role: "member"},
		{name: "a viewer never does", isolation: workspacesession.IsolationContainer, role: "viewer"},
		{
			name:      "an admin does not read a user-isolation recording",
			isolation: workspacesession.IsolationUser,
			role:      "admin",
		},
		{name: "an owner does read a user-isolation recording", isolation: workspacesession.IsolationUser, role: "owner", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			credential := hubauth.Credential{
				HostedRole: tt.role,
				Hosted:     &auth.HostedIdentity{Subject: tt.subject},
			}
			record := RecordingRecord{TerminalRecording: TerminalRecording{
				Isolation: tt.isolation, Subject: "user_ran_it",
			}}
			if got := RecordingReadable(credential, record); got != tt.want {
				t.Fatalf("terminalRecordingReadable() = %v, want %v", got, tt.want)
			}
		})
	}
}
