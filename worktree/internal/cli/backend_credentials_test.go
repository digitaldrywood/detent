package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
)

func TestCodexCredentialPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		command  string
		env      map[string]string
		home     string
		wantRoot string
	}{
		{
			name:     "command assignment overrides inherited home",
			command:  "env CODEX_HOME=/opt/detent/codex-high codex app-server",
			env:      map[string]string{"CODEX_HOME": "/var/lib/codex"},
			home:     "/home/operator",
			wantRoot: "/opt/detent/codex-high",
		},
		{
			name:     "quoted command assignment expands environment",
			command:  `CODEX_HOME="$HOME/codex profile" codex app-server`,
			env:      map[string]string{"HOME": "/home/operator"},
			home:     "/home/operator",
			wantRoot: "/home/operator/codex profile",
		},
		{
			name:     "inherited codex home",
			command:  "codex app-server",
			env:      map[string]string{"CODEX_HOME": "/var/lib/codex"},
			home:     "/home/operator",
			wantRoot: "/var/lib/codex",
		},
		{
			name:     "default user home",
			command:  "codex app-server",
			home:     "/home/operator",
			wantRoot: "/home/operator/.codex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lookup := func(name string) (string, bool) {
				value, ok := tt.env[name]
				return value, ok
			}
			got, err := codexCredentialPath(tt.command, lookup, func() (string, error) {
				return tt.home, nil
			})
			if err != nil {
				t.Fatalf("codexCredentialPath() error = %v", err)
			}
			wantRoot, err := filepath.Abs(filepath.FromSlash(tt.wantRoot))
			if err != nil {
				t.Fatalf("resolve expected credential root: %v", err)
			}
			want := filepath.Join(wantRoot, "auth.json")
			if got != want {
				t.Fatalf("codexCredentialPath() = %q, want %q", got, want)
			}
		})
	}
}

func TestBackendCredentialFileWatcherNotifiesOnReplacement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte("first account"), 0o600); err != nil {
		t.Fatalf("write initial auth file: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	notified := make(chan struct{}, 2)
	done, err := startBackendCredentialFileWatcherWithRetry(ctx, path, func(context.Context) {
		notified <- struct{}{}
	}, nil, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("startBackendCredentialFileWatcher() error = %v", err)
	}

	replacement := filepath.Join(dir, "auth.json.replacement")
	if err := os.WriteFile(replacement, []byte("replacement account"), 0o600); err != nil {
		t.Fatalf("write replacement auth file: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replace auth file: %v", err)
	}

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("credential replacement did not trigger watcher notification")
	}
	select {
	case <-notified:
		t.Fatal("one credential replacement triggered duplicate notifications")
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("credential watcher did not stop after cancellation")
	}
}

func TestBackendCredentialFileWatcherRetriesMissingDirectory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing", "codex", "auth.json")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	notified := make(chan struct{}, 1)
	done, err := startBackendCredentialFileWatcherWithRetry(ctx, path, func(context.Context) {
		notified <- struct{}{}
	}, nil, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("startBackendCredentialFileWatcher() error = %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create credential directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("new account"), 0o600); err != nil {
		t.Fatalf("create credential file: %v", err)
	}

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("credential creation after missing directory did not trigger watcher notification")
	}
	select {
	case <-notified:
		t.Fatal("credential creation after missing directory triggered duplicate notifications")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("credential watcher did not stop after cancellation")
	}
}

func TestBackendCredentialReconciliationCoalescesRetryAttachmentAndFilesystemEvent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("new account"), 0o600); err != nil {
		t.Fatalf("create credential file: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	updates := make(chan configwatcher.FileUpdate[credentialFileStamp], 2)
	notified := make(chan struct{}, 2)
	timer := newControlledBackendCredentialWatchTimer()
	done := make(chan bool, 1)
	var lastStamp *credentialFileStamp
	go func() {
		done <- monitorBackendCredentialFile(
			ctx,
			path,
			updates,
			func(context.Context) { notified <- struct{}{} },
			nil,
			time.Hour,
			time.Hour,
			&lastStamp,
			func(time.Duration) backendCredentialWatchTimer { return timer },
		)
	}()

	timer.waitForReset(t)
	updates <- configwatcher.FileUpdate[credentialFileStamp]{
		Path:  path,
		Value: credentialFileStamp{size: 1},
	}
	timer.waitForReset(t)
	timer.fire(t)

	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("credential creation while detached did not trigger notification")
	}

	updates <- configwatcher.FileUpdate[credentialFileStamp]{
		Path:  path,
		Value: credentialFileStamp{size: 2},
	}
	timer.waitForReset(t)
	timer.fire(t)
	select {
	case <-notified:
		t.Fatal("retry attachment and filesystem event triggered duplicate notifications")
	case <-time.After(25 * time.Millisecond):
	}

	cancel()
	select {
	case monitoring := <-done:
		if monitoring {
			t.Fatal("credential monitor requested retry after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("credential monitor did not stop after cancellation")
	}
}

type controlledBackendCredentialWatchTimer struct {
	ticks  chan time.Time
	resets chan time.Duration
}

func newControlledBackendCredentialWatchTimer() *controlledBackendCredentialWatchTimer {
	return &controlledBackendCredentialWatchTimer{
		ticks:  make(chan time.Time, 1),
		resets: make(chan time.Duration, 4),
	}
}

func (t *controlledBackendCredentialWatchTimer) C() <-chan time.Time {
	return t.ticks
}

func (t *controlledBackendCredentialWatchTimer) Reset(delay time.Duration) bool {
	t.resets <- delay
	return true
}

func (t *controlledBackendCredentialWatchTimer) Stop() bool {
	return true
}

func (t *controlledBackendCredentialWatchTimer) waitForReset(testingT *testing.T) {
	testingT.Helper()
	select {
	case <-t.resets:
	case <-time.After(time.Second):
		testingT.Fatal("timed out waiting for credential reconciliation reset")
	}
}

func (t *controlledBackendCredentialWatchTimer) fire(testingT *testing.T) {
	testingT.Helper()
	select {
	case t.ticks <- time.Now():
	case <-time.After(time.Second):
		testingT.Fatal("timed out firing credential reconciliation timer")
	}
}
