package hubserver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"unicode/utf8"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var registerNativeSearchText = sync.OnceValue(func() error {
	if err := registerNativeSearchTokenizer(); err != nil {
		return err
	}
	return sqlite.RegisterDeterministicScalarFunction("native_search_text", 2, func(_ *sqlite.FunctionContext, values []driver.Value) (driver.Value, error) {
		content, contentOK := values[0].(string)
		scope, scopeOK := values[1].(string)
		if !contentOK || !scopeOK {
			return nil, errors.New("invalid search text input")
		}
		return nativeSearchText(content, scope), nil
	})
})

func nativeSearchText(content, scope string) string {
	return scope + "_" + content
}

func nativeSearchBytePrefix(content string, budget int) string {
	if len(content) <= budget {
		return content
	}
	for budget > 0 && !utf8.RuneStart(content[budget]) {
		budget--
	}
	return content[:budget]
}

func nativeSearchIndexPending(scope nativeScope) (string, []any) {
	return `EXISTS(SELECT 1 FROM native_work_search_rebuild r
 WHERE r.through_id<r.last_id AND EXISTS(SELECT 1 FROM (
 SELECT d.id FROM (SELECT id FROM issues WHERE organization_id=? AND project_id=?
 LIMIT ` + strconv.Itoa(nativeSearchPostingBudget) + `) i CROSS JOIN native_work_documents d
 WHERE d.issue_id=i.id AND d.id>r.through_id AND d.id<=r.last_id
 LIMIT ` + strconv.Itoa(nativeSearchPostingBudget) + `) d WHERE (NOT EXISTS(SELECT 1 FROM native_work_scoped_fts WHERE rowid=d.id*2)
 OR NOT EXISTS(SELECT 1 FROM native_work_search_lengths WHERE document_id=d.id))))
 OR EXISTS(SELECT 1 FROM native_work_search_dirty WHERE organization_id=? AND project_id=?)
 OR (? AND EXISTS(SELECT 1 FROM issues WHERE organization_id=? AND project_id=? AND actor_principal_id IS NULL))`,
		[]any{scope.organization, scope.project, scope.organization, scope.project, scope.credential.HostedRole == "reporter", scope.organization, scope.project}
}

func requireNativeSearchIndex(ctx context.Context, q nativeQueryer, scope nativeScope) error {
	predicate, args := nativeSearchIndexPending(scope)
	var pending bool
	if err := q.QueryRowContext(ctx, "SELECT "+predicate, args...).Scan(&pending); err != nil {
		return fmt.Errorf("read search index progress: %w", err)
	}
	if pending {
		return fmt.Errorf("search index rebuilding: %w", operatortool.ErrServiceUnavailable)
	}
	return nil
}

type tenantSearchRepository struct {
	tenantstore.Repository
}

func (r tenantSearchRepository) ReadSearch(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(searchstore.Reader) error) error {
	return r.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error { return read(reader) })
}

func (r tenantSearchRepository) WriteSearch(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(searchstore.Writer) error) error {
	return r.WriteTenant(ctx, organization, workload, func(writer tenantstore.Writer) error { return write(writer) })
}

func (s *Service) maintainNativeSearch(ctx context.Context) {
	_, err := searchstore.NewContent(s.runDetailStore()).RefreshOne(s.runDetailContext(ctx), tenantSearchRepository{s.config.TenantRepository}, s.accessOrganization())
	if err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "rebuild scoped search index", "error", err)
	}
}
