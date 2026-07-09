package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	storepkg "github.com/digitaldrywood/detent/internal/store"
)

func TestCLIAPIKeyErrorAddsBusyHint(t *testing.T) {
	t.Parallel()

	busyErr := fmt.Errorf("creating api key: %w", generateSQLiteBusyError(t))

	tests := []struct {
		name         string
		err          error
		wantContains string
	}{
		{
			name:         "sqlite busy gets retry hint",
			err:          busyErr,
			wantContains: "holding the runtime database write lock",
		},
		{
			name:         "validation error stays validation",
			err:          apikey.ErrNameRequired,
			wantContains: apikey.ErrNameRequired.Error(),
		},
		{
			name:         "other errors pass through",
			err:          errors.New("boom"),
			wantContains: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cliAPIKeyError(tt.err)
			if got == nil {
				t.Fatal("cliAPIKeyError() = nil, want error")
			}
			if !strings.Contains(got.Error(), tt.wantContains) {
				t.Fatalf("cliAPIKeyError() = %q, want substring %q", got.Error(), tt.wantContains)
			}
			if !errors.Is(got, tt.err) && got.Error() != tt.err.Error() {
				t.Fatalf("cliAPIKeyError() = %v does not wrap %v", got, tt.err)
			}
		})
	}

	if hinted := cliAPIKeyError(busyErr); !storepkg.IsBusy(hinted) {
		t.Fatalf("cliAPIKeyError() busy result no longer matches IsBusy: %v", hinted)
	}
}

// generateSQLiteBusyError produces a genuine driver SQLITE_BUSY error by
// holding a write transaction on one connection while a second, zero-timeout
// connection attempts a write.
func generateSQLiteBusyError(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "busy.db")

	holder, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	holderConn, err := holder.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	t.Cleanup(func() { _ = holderConn.Close() })
	if _, err := holderConn.ExecContext(ctx, "create table if not exists busy_probe (id integer)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := holderConn.ExecContext(ctx, "begin immediate"); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	t.Cleanup(func() { _, _ = holderConn.ExecContext(ctx, "rollback") })

	contender, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	if _, err := contender.ExecContext(ctx, "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatalf("set contender busy_timeout: %v", err)
	}
	_, busyErr := contender.ExecContext(ctx, "insert into busy_probe (id) values (1)")
	if busyErr == nil {
		t.Fatal("expected contender write to fail with SQLITE_BUSY")
	}
	return busyErr
}
