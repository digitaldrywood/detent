package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubSecretsRetiredRotation(t *testing.T) {
	for _, args := range [][]string{nil, {"rotate"}, {"rotate", "--database", filepath.Join(t.TempDir(), "hub.db")}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := newHubSecretsCommand()
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(args)
			err := cmd.Execute()
			message := output.String()
			if err != nil {
				message += err.Error()
			}
			if !strings.Contains(message, "restart") || !strings.Contains(message, "secrets/status") {
				t.Fatalf("missing rewrap procedure: %s", message)
			}
			if len(args) > 2 {
				if _, err := os.Stat(args[2]); !os.IsNotExist(err) {
					t.Fatalf("retired command accessed database: %v", err)
				}
			}
		})
	}
}
