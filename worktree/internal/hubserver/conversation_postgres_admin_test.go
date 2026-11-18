package hubserver

import (
	"database/sql"
	"testing"
)

func conversationTestAdmin(t *testing.T, service *Service) *sql.DB {
	t.Helper()
	admin, ok := projectRecordDatabases.Load(service.config.TenantRepository)
	if !ok {
		t.Fatal("conversation fixture has no isolated PostgreSQL administrator")
	}
	return admin.(*sql.DB)
}
