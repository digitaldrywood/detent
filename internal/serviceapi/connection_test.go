package serviceapi

import (
	"bytes"
	"reflect"
	"testing"
)

func TestConnectionEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		connection Connection
		want       map[string]string
	}{
		{
			name: "unset clears ambient service credentials",
			want: RestrictedEnvironment(),
		},
		{
			name:       "address and token",
			connection: Connection{Address: "100.111.222.33:4100", DispositionToken: "worker-token"},
			want: map[string]string{
				AddressEnvironment:          "100.111.222.33:4100",
				TokenEnvironment:            "",
				DispositionTokenEnvironment: "worker-token",
			},
		},
		{
			name:       "address without capability clears ambient tokens",
			connection: Connection{Address: "127.0.0.1:4100"},
			want: map[string]string{
				AddressEnvironment:          "127.0.0.1:4100",
				TokenEnvironment:            "",
				DispositionTokenEnvironment: "",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.connection.Environment(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Connection.Environment() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestWorkerCredentialsAreProjectScopedAndProcessLocal(t *testing.T) {
	t.Parallel()

	first, err := newWorkerCredentials(bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatalf("newWorkerCredentials(first) error = %v", err)
	}
	second, err := newWorkerCredentials(bytes.NewReader(bytes.Repeat([]byte{2}, 32)))
	if err != nil {
		t.Fatalf("newWorkerCredentials(second) error = %v", err)
	}
	token := first.Token("detent")

	tests := []struct {
		name        string
		credentials *WorkerCredentials
		token       string
		wantProject string
		wantOK      bool
	}{
		{name: "matching process and project", credentials: first, token: token, wantProject: "detent", wantOK: true},
		{name: "different process secret", credentials: second, token: token},
		{name: "tampered project", credentials: first, token: first.Token("detent") + "x"},
		{name: "ordinary API token", credentials: first, token: "detent_operator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			credential, ok := tt.credentials.Authenticate(tt.token)
			if ok != tt.wantOK || credential.ProjectID != tt.wantProject {
				t.Fatalf("Authenticate() = %#v, %t; want project %q, ok %t", credential, ok, tt.wantProject, tt.wantOK)
			}
		})
	}
}

func TestWorkerCredentialsRejectEmptyProject(t *testing.T) {
	t.Parallel()

	credentials, err := NewWorkerCredentials()
	if err != nil {
		t.Fatalf("NewWorkerCredentials() error = %v", err)
	}
	if token := credentials.Token(" \t"); token != "" {
		t.Fatalf("Token(empty) = %q, want empty", token)
	}
}
