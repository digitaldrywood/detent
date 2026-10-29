package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeSearchRebuildBatch = 100

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

type nativeSearchPostingBatch struct {
	statement string
	args      []any
}

type nativeSearchDocument struct {
	id, revision                     int64
	issue                            int64
	comment                          sql.NullString
	organization, project, principal string
	archived                         int
	title, body                      string
	tokens                           int64
	scopes                           []string
	batches                          []nativeSearchPostingBatch
	ftsArgs                          []any
}

func (d *database) prepareNativeSearchDocument(ctx context.Context, id int64) (nativeSearchDocument, error) {
	document := nativeSearchDocument{id: id}
	var postings string
	err := d.reader.QueryRowContext(ctx, `SELECT d.search_revision,d.issue_id,d.comment_id,i.organization_id,i.project_id,i.archived,COALESCE(i.actor_principal_id,''),
 c.title,c.body,native_search_tokens(c.title,'')+native_search_tokens(c.body,''),native_search_postings(c.title,c.body)
 FROM native_work_documents d JOIN issues i ON i.id=d.issue_id JOIN native_work_search_content c ON c.id=d.id WHERE d.id=?`, id).Scan(
		&document.revision, &document.issue, &document.comment, &document.organization, &document.project, &document.archived, &document.principal, &document.title, &document.body, &document.tokens, &postings)
	if err != nil {
		return document, err
	}
	scope := nativeScope{organization: tracker.OrganizationID(document.organization), project: tracker.ProjectID(document.project)}
	archive := strconv.FormatBool(document.archived != 0)
	document.scopes = nativeSearchScopes(scope, archive)
	scope.credential.HostedRole = "reporter"
	scope.sourceActor = &tracker.Actor{PrincipalID: document.principal}
	document.scopes = append(document.scopes, nativeSearchScopes(scope, archive)...)
	if document.principal == "" {
		prefix := document.scopes[0]
		document.scopes[1] = strings.TrimSuffix(prefix, "v") + "rv"
	}
	for variant, scope := range document.scopes {
		document.ftsArgs = append(document.ftsArgs, document.id*2+int64(variant), nativeSearchText(document.title, scope), nativeSearchText(document.body, scope), scope)
	}
	records := strings.Fields(postings)
	for _, scope := range document.scopes {
		for start := 0; start < len(records); start += 200 {
			end := min(start+200, len(records))
			values := make([]string, 0, end-start)
			args := make([]any, 0, (end-start)*5)
			for _, record := range records[start:end] {
				count, term, ok := strings.Cut(record, ":")
				frequency, err := strconv.ParseInt(count, 10, 64)
				if !ok || err != nil {
					return document, errors.New("invalid search posting record")
				}
				values = append(values, "(?,?,?,?,?)")
				args = append(args, scope, term, id, frequency, float64(frequency)/float64(1+document.tokens))
			}
			document.batches = append(document.batches, nativeSearchPostingBatch{
				statement: "INSERT INTO native_work_search_postings(scope,term,document_id,frequency,impact) VALUES " + strings.Join(values, ","), args: args,
			})
		}
	}
	return document, nil
}

func (d *database) publishNativeSearchDocument(ctx context.Context, tx *sql.Tx, document nativeSearchDocument) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE native_work_documents SET search_revision=search_revision WHERE id=? AND search_revision=? AND issue_id=? AND comment_id IS ?
 AND EXISTS(SELECT 1 FROM native_work_search_content WHERE id=native_work_documents.id AND title=? AND body=?)
 AND EXISTS(SELECT 1 FROM issues WHERE id=native_work_documents.issue_id AND organization_id=? AND project_id=? AND archived=? AND COALESCE(actor_principal_id,'')=?)`,
		document.id, document.revision, document.issue, document.comment, document.title, document.body, document.organization, document.project, document.archived, document.principal)
	if err != nil {
		return false, fmt.Errorf("validate search document revision: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO native_work_search_refresh SELECT d.id,i.organization_id,i.project_id FROM native_work_documents d JOIN issues i ON i.id=d.issue_id WHERE d.id=?
 ON CONFLICT(document_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id`, document.id)
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_search_lengths WHERE document_id=?`, document.id); err != nil {
		return false, fmt.Errorf("remove stale search postings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_work_search_lengths(document_id,scope,tokens) VALUES(?,?,?),(?,?,?)`,
		document.id, document.scopes[0], document.tokens, document.id, document.scopes[1], document.tokens); err != nil {
		return false, fmt.Errorf("index search lengths: %w", err)
	}
	for _, batch := range document.batches {
		if _, err := tx.ExecContext(ctx, batch.statement, batch.args...); err != nil {
			return false, fmt.Errorf("index search postings: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_scoped_fts WHERE rowid IN(?,?)`, document.id*2, document.id*2+1); err != nil {
		return false, fmt.Errorf("remove stale search document: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_work_scoped_fts(rowid,title,body,scope) VALUES(?,?,?,?),(?,?,?,?)`,
		document.ftsArgs...); err != nil {
		return false, fmt.Errorf("index search document: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_search_refresh WHERE document_id=?`, document.id); err != nil {
		return false, fmt.Errorf("complete search refresh: %w", err)
	}
	return true, nil
}

func (d *database) backfillNativeSearch(ctx context.Context) (bool, error) {
	var issue, through, last, next int64
	queued := true
	err := d.reader.QueryRowContext(ctx, `SELECT document_id FROM native_work_search_refresh ORDER BY document_id LIMIT 1`).Scan(&next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("select search refresh: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		queued = false
		err = d.reader.QueryRowContext(ctx, `SELECT issue_id,through_id FROM native_work_search_dirty ORDER BY issue_id LIMIT 1`).Scan(&issue, &through)
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
	}
	var document nativeSearchDocument
	if next != 0 {
		document, err = d.prepareNativeSearchDocument(ctx, next)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return true, nil
			}
			return false, fmt.Errorf("prepare search document: %w", err)
		}
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if next != 0 {
		published, err := d.publishNativeSearchDocument(ctx, tx, document)
		if err != nil {
			return false, err
		}
		if !published {
			return true, d.commit(ctx, tx)
		}
	}
	if queued {
		return true, d.commit(ctx, tx)
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
