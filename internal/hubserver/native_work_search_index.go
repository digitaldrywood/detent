package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

const nativeSearchRebuildBatch = 100

var registerNativeSearchText = sync.OnceValue(func() error {
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
	var indexed strings.Builder
	inside := false
	for _, r := range content {
		codepoint := r
		if r == 0xfffe || r == 0xffff {
			codepoint = 0xfffd
		}
		token := sort.Search(len(nativeSearchTokenBoundaries), func(i int) bool { return nativeSearchTokenBoundaries[i] > codepoint })%2 == 1
		diacritic := r >= 768 && r <= 817 && (r < 800 && uint32(0x08029FDF)&(1<<uint(r-768)) != 0 || r >= 800 && uint32(0x000361F8)&(1<<uint(r-800)) != 0)
		if token && !inside {
			indexed.WriteString(scope)
			indexed.WriteByte(' ')
		}
		inside = token || inside && diacritic
		indexed.WriteRune(r)
	}
	return indexed.String()
}

func nativeSearchIndexPending(scope nativeScope) (string, []any) {
	return `EXISTS(SELECT 1 FROM native_work_search_rebuild r
 WHERE r.through_id<r.last_id AND EXISTS(SELECT 1 FROM issues i CROSS JOIN native_work_documents d
 WHERE i.organization_id=? AND i.project_id=? AND d.issue_id=i.id AND d.id>r.through_id AND d.id<=r.last_id
 AND NOT EXISTS(SELECT 1 FROM native_work_scoped_fts WHERE rowid=d.id*2)))
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

func (s *Service) maintainNativeSearch(ctx context.Context) {
	for range nativeSearchRebuildBatch {
		more, err := s.database.backfillNativeSearch(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.ErrorContext(ctx, "rebuild scoped search index", "error", err)
		}
		if err != nil || !more {
			return
		}
	}
}

func (d *database) backfillNativeSearch(ctx context.Context) (bool, error) {
	var issue, through, last, next int64
	err := d.reader.QueryRowContext(ctx, `SELECT issue_id,through_id FROM native_work_search_dirty ORDER BY issue_id LIMIT 1`).Scan(&issue, &through)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("select search scope update: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if err := d.reader.QueryRowContext(ctx, `SELECT through_id,last_id FROM native_work_search_rebuild WHERE id=1`).Scan(&through, &last); err != nil {
			return false, fmt.Errorf("select search rebuild cursor: %w", err)
		}
		if through >= last {
			return false, nil
		}
		err = d.reader.QueryRowContext(ctx, `SELECT id FROM native_work_documents WHERE id>? AND id<=? ORDER BY id LIMIT 1`, through, last).Scan(&next)
	} else {
		err = d.reader.QueryRowContext(ctx, `SELECT id FROM native_work_documents WHERE issue_id=? AND id>? ORDER BY id LIMIT 1`, issue, through).Scan(&next)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("select search document: %w", err)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if next != 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_scoped_fts WHERE rowid IN(?,?)`, next*2, next*2+1); err != nil {
			return false, fmt.Errorf("remove stale search document: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO native_work_scoped_fts(rowid,title,body,scope) SELECT id,title,body,scope FROM native_work_scoped_content WHERE source_id=?`, next); err != nil {
			return false, fmt.Errorf("index search document: %w", err)
		}
	}
	if issue == 0 {
		if next == 0 {
			next = last
		}
		_, err = tx.ExecContext(ctx, `UPDATE native_work_search_rebuild SET through_id=? WHERE id=1 AND through_id=?`, next, through)
	} else if next == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM native_work_search_dirty WHERE issue_id=? AND through_id=?`, issue, through)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE native_work_search_dirty SET through_id=? WHERE issue_id=? AND through_id=?`, next, issue, through)
	}
	if err != nil {
		return false, fmt.Errorf("advance search rebuild cursor: %w", err)
	}
	return true, d.commit(ctx, tx)
}
