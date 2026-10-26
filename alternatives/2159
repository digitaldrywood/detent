package hubserver

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSpritePoolBootstrapShell(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, setup     string
		installer, want int
	}{
		{"blank", "", 0, 0},
		{"whitespace", " \n\t", 0, 0},
		{"carriage return", " \r\n\t", 0, 0},
		{"no-op", ":", 0, 0},
		{"customer command", "printf 'customer-secret\\n'; touch \"$HOME/customer-ran\"", 0, 0},
		{"customer failure", "printf 'customer-secret\\n'; exit 23", 0, 23},
		{"installer failure", ":", 100, 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			installer := filepath.Join(root, "installer")
			if err := os.WriteFile(installer, []byte("cat >/dev/null\nprintf 'Installing PostgreSQL client 18.6\\n'\nif [[ "+strconv.Itoa(test.installer)+" != 0 ]]; then printf \"E: Version 'installer-secret' for 'postgresql-client-17' was not found\\n\" >&2; fi\nprintf 'Authorization: Bearer provider-secret\\n' >&2\nexit "+strconv.Itoa(test.installer)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			curl := "#!/usr/bin/env bash\nwhile [[ $1 != --output ]]; do shift; done\ncp -- \"$FIXTURE_INSTALLER\" \"$2\"\n"
			if err := os.WriteFile(filepath.Join(root, "curl"), []byte(curl), 0700); err != nil {
				t.Fatal(err)
			}
			script := spritePoolBootstrapScript("org_example", "https://example.com", test.setup, "enrollment-secret", "v0.1.0")
			cmd := exec.CommandContext(t.Context(), "bash", "-s")
			cmd.Env = append(os.Environ(), "HOME="+root, "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURE_INSTALLER="+installer)
			cmd.Stdin = strings.NewReader(script)
			output, err := cmd.CombinedOutput()
			status := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				status = exit.ExitCode()
			}
			if status != test.want {
				t.Fatalf("exit=%d, want=%d; output=%s", status, test.want, output)
			}
			if strings.Contains(string(output), "customer-secret") || strings.Contains(string(output), "enrollment-secret") {
				t.Fatal("bootstrap exposed sensitive input")
			}
			if strings.Contains(string(output), "Runner bootstrap completed") != (test.want == 0) {
				t.Fatalf("incorrect completion: %s", output)
			}
			_, markerErr := os.Stat(filepath.Join(root, "customer-ran"))
			if (markerErr == nil) != (test.name == "customer command") {
				t.Fatalf("customer command did not retain its behavior: %v", markerErr)
			}
			var log strings.Builder
			for _, line := range strings.Split(string(output), "\n") {
				if safe := spriteBootstrapDiagnostic(line); safe != "" {
					log.WriteString(safe + "\n")
				}
			}
			for _, secret := range []string{"customer-secret", "enrollment-secret", "installer-secret", "provider-secret"} {
				if strings.Contains(log.String(), secret) {
					t.Fatal("sensitive marker reached returned bootstrap logs")
				}
			}
			if test.installer != 0 && !strings.Contains(log.String(), "Runner installer: APT requested package version unavailable") {
				t.Fatalf("installer stderr diagnostic discarded: %s", log.String())
			}
			t.Logf("exit=%d; retained stdout/stderr after allowlisting:\n%s", status, log.String())
		})
	}
}
