package cloudentry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
)

type TenantSpec struct {
	Organization Organization
	Directory    string
	Socket       string
	PublicURL    string
	Issuer       string
	PublicKey    string
}

type Launcher interface {
	Start(context.Context, TenantSpec) error
	Stop(string) error
	Close() error
}

type ExecLauncher struct {
	Binary      string
	Environment []string
	Configure   func(TenantSpec) ([]byte, error)
	Logger      *slog.Logger

	mu      sync.Mutex
	running map[string]*supervisedTenant
}

type supervisedTenant struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (l *ExecLauncher) Start(_ context.Context, spec TenantSpec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running == nil {
		l.running = make(map[string]*supervisedTenant)
	}
	if _, ok := l.running[spec.Organization.ID]; ok {
		return nil
	}
	config, err := l.Configure(spec)
	if err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(spec.Directory, "tenant.yaml"), config); err != nil {
		return err
	}
	token, err := tenantAdminToken(spec.Directory)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	tenant := &supervisedTenant{cancel: cancel, done: make(chan struct{})}
	l.running[spec.Organization.ID] = tenant
	go l.supervise(ctx, spec, token, tenant.done)
	return nil
}

func (l *ExecLauncher) supervise(ctx context.Context, spec TenantSpec, token string, done chan struct{}) {
	defer close(done)
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		cmd := exec.CommandContext(ctx, l.Binary, "hub", "serve", "--hosted-config", filepath.Join(spec.Directory, "tenant.yaml"),
			"--database", filepath.Join(spec.Directory, "hub.db"), "--listen", "unix:"+spec.Socket, "--github-disabled") // #nosec G204 -- the operator-configured Detent binary runs with fixed arguments and no shell.
		cmd.Env = append(append(baseEnvironment(), l.Environment...), "DETENT_HUB_ADMIN_TOKEN="+token)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 15 * time.Second
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 5*time.Minute {
			backoff = time.Second
		}
		l.logger().Warn("tenant Hub exited; restarting", "organization", spec.Organization.ID, "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (l *ExecLauncher) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

func (l *ExecLauncher) Stop(id string) error {
	l.mu.Lock()
	tenant, ok := l.running[id]
	delete(l.running, id)
	l.mu.Unlock()
	if !ok {
		return nil
	}
	tenant.cancel()
	select {
	case <-tenant.done:
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("tenant %s did not stop", id)
	}
}

func (l *ExecLauncher) Close() error {
	l.mu.Lock()
	ids := make([]string, 0, len(l.running))
	for id := range l.running {
		ids = append(ids, id)
	}
	l.mu.Unlock()
	var err error
	for _, id := range ids {
		err = errors.Join(err, l.Stop(id))
	}
	return err
}

func baseEnvironment() []string {
	var result []string
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "TZ"} {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	return result
}

func writePrivateFile(path string, contents []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, contents, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func tenantAdminToken(directory string) (string, error) {
	path := filepath.Join(directory, "admin-token")
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		return string(raw), nil
	} else if !errors.Is(err, os.ErrNotExist) && err != nil {
		return "", err
	}
	token, err := apikey.GenerateToken()
	if err != nil {
		return "", err
	}
	return token, writePrivateFile(path, []byte(token))
}
