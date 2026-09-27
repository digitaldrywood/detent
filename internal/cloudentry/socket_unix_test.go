//go:build !windows

package cloudentry

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyPrivateSocket(t *testing.T) {
	t.Parallel()
	private, err := os.MkdirTemp("", "entry")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(private) })
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(private, "public")
	if err := os.Mkdir(public, 0o700); err != nil {
		t.Fatal(err)
	}
	var listenConfig net.ListenConfig
	for _, path := range []string{filepath.Join(private, "t.sock"), filepath.Join(public, "t.sock")} {
		listener, err := listenConfig.Listen(t.Context(), "unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
	}
	if err := os.Chmod(public, 0o755); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(private, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
		ok   bool
	}{
		{"private socket", filepath.Join(private, "t.sock"), true},
		{"shared directory", filepath.Join(public, "t.sock"), false},
		{"regular file", regular, false},
		{"missing", filepath.Join(private, "missing.sock"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := verifyPrivateSocket(test.path); (err == nil) != test.ok {
				t.Fatalf("verifyPrivateSocket error = %v, want ok %v", err, test.ok)
			}
		})
	}
}
