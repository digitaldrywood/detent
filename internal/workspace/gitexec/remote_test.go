package gitexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyPublicationRemoteRepository(t *testing.T) {
	checkout := t.TempDir()
	local := filepath.Join(t.TempDir(), "checkout.git")
	otherLocal := filepath.Join(filepath.Dir(local), "other.git")
	for _, path := range []string{local, otherLocal} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	if _, err := Run(ctx, checkout, "init"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, destination, fetch, push, refusal string
	}{
		{name: "equivalent GitHub remotes", destination: "https://github.com/Example/Repo.git", fetch: "git@github.com:example/repo.git", push: "https://github.com/example/REPO"},
		{name: "different GitHub fetch", destination: "https://github.com/Example/Repo.git", fetch: "https://github.com/example/other", push: "https://github.com/example/repo", refusal: "fetch remote differs"},
		{name: "different GitHub push", destination: "https://github.com/example/repo.git", fetch: "https://github.com/example/repo", push: "git@github.com:other/repo.git", refusal: "push remote differs"},
		{name: "different case on another host", destination: "https://example.com/Example/Repo", fetch: "https://example.com/example/repo", refusal: "fetch remote differs"},
		{name: "exact local path", destination: local, fetch: local, push: local},
		{name: "different local path", destination: local, fetch: local, push: otherLocal, refusal: "push remote differs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for key, value := range map[string]string{"remote.origin.url": test.fetch, "remote.origin.pushurl": test.push} {
				if _, err := Run(ctx, checkout, "config", key, value); err != nil {
					t.Fatal(err)
				}
			}
			err := VerifyPublicationRemote(ctx, checkout, "origin", PublicationRepository("", test.destination))
			if test.refusal == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.refusal) {
				t.Fatalf("verification error = %v, want %q", err, test.refusal)
			}
		})
	}
}
