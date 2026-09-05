package runnerauth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
)

func TestPrivateIdentityLifecycle(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := Initialize(path, "https://hub.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	if !file.Identity.Valid() || !ValidCredential(file.Credential) || file.HubURL != "https://hub.example.test" {
		t.Fatal("invalid generated identity")
	}
	if _, err := Initialize(path, "https://hub.example.test"); err == nil {
		t.Fatal("initialization overwrote an existing identity")
	}
	loaded, err := Load(path)
	if err != nil || loaded.Credential != file.Credential || loaded.Identity.Binding != file.Identity.Binding {
		t.Fatalf("identity restart failed: %v", err)
	}
	file.PendingCredential, err = apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, file); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(path)
	if err != nil || loaded.PendingCredential != file.PendingCredential {
		t.Fatalf("pending credential not durable: %v", err)
	}
	file.Credential, file.PendingCredential = file.PendingCredential, ""
	if err := Save(path, file); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(path)
	if err != nil || loaded.Credential != file.Credential || loaded.PendingCredential != "" {
		t.Fatalf("rotation not durable: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*File)
	}{
		{"machine replacement", func(f *File) { f.Identity.MachineID = NewBinding().MachineID }},
		{"runner replacement", func(f *File) { f.Identity.RunnerID = NewBinding().RunnerID }},
		{"Hub replacement", func(f *File) { f.HubURL = "https://other.example.test" }},
		{"invalid credential", func(f *File) { f.Credential = "invalid" }},
		{"invalid pending credential", func(f *File) { f.PendingCredential = "invalid" }},
		{"unknown schema", func(f *File) { f.Schema = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := file
			test.change(&copy)
			if err := Save(path, copy); err == nil {
				t.Fatal("unsafe identity change accepted")
			}
		})
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary credential files remain: count=%d err=%v", len(entries), err)
	}
}

func TestIdentityFilesRejectUnsafeInput(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*testing.T, string)
	}{
		{"malformed JSON", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("invalid-private-credential"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown fields", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(`{"provider_key":"private-provider-key"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized file", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(strings.Repeat("x", 33<<10)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra JSON", func(t *testing.T, path string) {
			t.Helper()
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(" {}"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"public file", func(t *testing.T, path string) {
			t.Helper()
			if runtime.GOOS == "windows" {
				t.Skip("Unix permissions")
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"public directory", func(t *testing.T, path string) {
			t.Helper()
			if runtime.GOOS == "windows" {
				t.Skip("Unix permissions")
			}
			if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			if _, err := Initialize(path, "https://hub.example.test"); err != nil {
				t.Fatal(err)
			}
			test.change(t, path)
			if _, err := Load(path); err == nil || strings.Contains(err.Error(), "private-provider-key") || strings.Contains(err.Error(), "invalid-private-credential") {
				t.Fatal("unsafe file accepted or error exposed content")
			}
		})
	}
}

func TestRunnerTransportURLPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		url   string
		valid bool
	}{
		{"https://hub.example.test", true}, {"http://127.0.0.1:1234", true}, {"http://[::1]:1234", true}, {"http://localhost:1234", true},
		{"http://hub.example.test", false}, {"https://user:secret@hub.example.test", false}, {"https://hub.example.test?token=secret", false}, {"https://hub.example.test#secret", false}, {"ftp://localhost", false}, {"relative", false},
	} {
		t.Run(test.url, func(t *testing.T) {
			if got := ValidateHubURL(test.url) == nil; got != test.valid {
				t.Fatalf("URL valid=%v, want %v", got, test.valid)
			}
		})
	}
}

func TestRunnerBindingsAndOperations(t *testing.T) {
	t.Parallel()
	for _, binding := range []Binding{{}, {RunnerID: "runner_bad", MachineID: NewBinding().MachineID}, {RunnerID: NewBinding().RunnerID, MachineID: "machine_invalid"}} {
		if binding.Valid() {
			t.Fatal("invalid binding accepted")
		}
	}
	for _, operations := range [][]string{nil, {Read, Read}, {"admin"}, {""}, {Read, Claim, Heartbeat, Events, Collaborate, "extra"}} {
		if ValidOperations(operations) {
			t.Fatalf("invalid operations accepted: %v", operations)
		}
	}
	if !ValidOperations([]string{Read, Claim, Heartbeat, Events, Collaborate}) {
		t.Fatal("valid operations rejected")
	}
}
