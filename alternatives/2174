package hubscope

import (
	"context"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type EntryKeyAuditContext struct{}

type EntryKeyAudit struct {
	Claims                       cloudassert.Claims `json:"-"`
	Operation, Project, Resource string             `json:"-"`
	RowID                        int64              `json:"-"`
}

func EntryKeyAuditFrom(ctx context.Context) *EntryKeyAudit {
	audit, ok := ctx.Value(EntryKeyAuditContext{}).(*EntryKeyAudit)
	if !ok {
		return nil
	}
	return audit
}

func (a *EntryKeyAudit) ReadOnly() bool {
	if a.Operation == http.MethodPost+" /internal/v1/keys/catalog" {
		return true
	}
	if definition, found := operatortool.Lookup(a.Operation); found {
		return definition.Annotations.ReadOnly
	}
	method, _, _ := strings.Cut(a.Operation, " ")
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}
