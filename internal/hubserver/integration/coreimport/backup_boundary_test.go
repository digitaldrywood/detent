package coreimport_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func TestAdminBackupBoundary(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	owner := f.User(t, "owner", "owner", "owner@example.test", "", "")
	output := filepath.Join(t.TempDir(), "snapshot.db")
	hubfixture.RequireCode(t, f.Request(t, owner, http.MethodPost, "/api/v2/admin/backup", map[string]string{"output": output}), http.StatusNotFound, "not_found")
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hosted backup created output: %v", err)
	}
}
