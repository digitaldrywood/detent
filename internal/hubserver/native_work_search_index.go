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
	"unicode/utf8"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeSearchRebuildBatch = 1
const nativeSearchDocumentBytes = 64 * 1024
const nativeSearchPruneBatch = 256
const nativeSearchLegacyPruneBatch = 32

var registerNativeSearchText = sync.OnceValue(func() error {
	if err := registerNativeSearchTokenizer(); err != nil {
		return err
	}
	if err := sqlite.RegisterDeterministicScalarFunction("native_machine_identity", 2, func(_ *sqlite.FunctionContext, values []driver.Value) (driver.Value, error) {
		body, _ := values[0].(string)
		mode, _ := values[1].(string)
		if mode == "origin" {
			if origin, ok := issueorigin.Parse(body); ok {
				return sha256Hex(origin.Fingerprint), nil
			}
			return "", nil
		}
		return issueorigin.DefectFingerprint(body), nil
	}); err != nil {
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
	var err error
	if s.config.TenantRepository != nil {
		_, err = searchstore.NewContent(s.runDetailStore()).RefreshOne(s.runDetailContext(ctx), tenantSearchRepository{s.config.TenantRepository}, s.accessOrganization())
	} else {
		_, err = s.searchRefresh(ctx)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "rebuild scoped search index", "error", err)
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
	contentArgs                      []any
}

func (d *database) prepareNativeSearchDocument(ctx context.Context, id int64) (nativeSearchDocument, error) {
	document := nativeSearchDocument{id: id}
	var postings string
	err := d.reader.QueryRowContext(ctx, `SELECT d.search_revision,d.issue_id,d.comment_id,i.organization_id,i.project_id,i.archived,COALESCE(i.actor_principal_id,''),
 c.title,c.body
 FROM native_work_documents d JOIN issues i ON i.id=d.issue_id JOIN native_work_search_content c ON c.id=d.id
 WHERE d.id=? AND (d.comment_id IS NULL OR EXISTS(SELECT 1 FROM native_comments WHERE id=d.comment_id))`, id).Scan(
		&document.revision, &document.issue, &document.comment, &document.organization, &document.project, &document.archived, &document.principal, &document.title, &document.body)
	if err != nil {
		return document, err
	}
	title := nativeSearchBytePrefix(document.title, nativeSearchDocumentBytes)
	body := nativeSearchBytePrefix(document.body, nativeSearchDocumentBytes-len(title))
	document.contentArgs = []any{document.id, title, body}
	if err := d.reader.QueryRowContext(ctx, `SELECT native_search_tokens(?,'')+native_search_tokens(?,''),native_search_postings(?,?)`,
		title, body, title, body).Scan(&document.tokens, &postings); err != nil {
		return document, fmt.Errorf("tokenize bounded search content: %w", err)
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
		document.ftsArgs = append(document.ftsArgs, document.id*2+int64(variant), nativeSearchText(title, scope), nativeSearchText(body, scope), scope)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_search_postings WHERE document_id=?`, document.id); err != nil {
		return false, fmt.Errorf("remove stale search postings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_search_lengths WHERE document_id=?`, document.id); err != nil {
		return false, fmt.Errorf("remove stale search lengths: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_work_search_lengths(document_id,scope,tokens,search_revision) VALUES(?,?,?,?),(?,?,?,?)`,
		document.id, document.scopes[0], document.tokens, document.revision, document.id, document.scopes[1], document.tokens, document.revision); err != nil {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_fts WHERE rowid=?`, document.id); err != nil {
		return false, fmt.Errorf("remove stale unscoped search document: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_work_fts(rowid,title,body) VALUES(?,?,?)`, document.contentArgs...); err != nil {
		return false, fmt.Errorf("index unscoped search document: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_search_refresh WHERE document_id=?`, document.id); err != nil {
		return false, fmt.Errorf("complete search refresh: %w", err)
	}
	return true, nil
}

type nativeSearchObsoletePosting struct {
	scope, term string
}

const nativeSearchPostingRetained = `EXISTS(SELECT 1 FROM native_work_search_lengths l
 JOIN native_work_documents d ON d.id=l.document_id JOIN issues i ON i.id=d.issue_id
 WHERE l.document_id=native_work_search_postings.document_id AND l.scope=native_work_search_postings.scope AND l.search_revision>=0
 AND (d.comment_id IS NULL OR EXISTS(SELECT 1 FROM native_comments WHERE id=d.comment_id)))`

const nativeSearchDocumentSourceExists = `EXISTS(SELECT 1 FROM native_work_documents d JOIN issues i ON i.id=d.issue_id
 WHERE d.id=? AND (d.comment_id IS NULL OR EXISTS(SELECT 1 FROM native_comments WHERE id=d.comment_id)))`

func (d *database) obsoleteNativeSearchPostings(ctx context.Context, id int64) ([]nativeSearchObsoletePosting, error) {
	const query = `SELECT scope,term FROM native_work_search_postings
 WHERE document_id=? AND NOT ` + nativeSearchPostingRetained + ` ORDER BY scope,term LIMIT ?`
	rows, err := d.reader.QueryContext(ctx, query, id, nativeSearchPruneBatch)
	if err != nil {
		return nil, fmt.Errorf("select obsolete search postings: %w", err)
	}
	defer rows.Close()
	var postings []nativeSearchObsoletePosting
	for rows.Next() {
		var posting nativeSearchObsoletePosting
		if err := rows.Scan(&posting.scope, &posting.term); err != nil {
			return nil, err
		}
		postings = append(postings, posting)
	}
	return postings, rows.Err()
}

func pruneNativeSearchPostings(ctx context.Context, tx *sql.Tx, id int64, postings []nativeSearchObsoletePosting) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_work_search_refresh SELECT d.id,i.organization_id,i.project_id
 FROM native_work_documents d JOIN issues i ON i.id=d.issue_id WHERE d.id=?
 ON CONFLICT(document_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id`, id); err != nil {
		return fmt.Errorf("queue pruned search document: %w", err)
	}
	for _, posting := range postings {
		if _, err := tx.ExecContext(ctx, `DELETE FROM native_work_search_postings WHERE document_id=? AND scope=? AND term=? AND NOT `+nativeSearchPostingRetained,
			id, posting.scope, posting.term); err != nil {
			return fmt.Errorf("prune obsolete search posting: %w", err)
		}
	}
	return nil
}

func (d *database) retireLegacyNativeSearch(ctx context.Context) (bool, error) {
	var exists bool
	if err := d.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='native_work_legacy_fts')`).Scan(&exists); err != nil {
		return false, fmt.Errorf("read legacy search retirement: %w", err)
	}
	if !exists {
		return false, nil
	}
	for _, batch := range []struct {
		pending, remove string
	}{
		{`SELECT EXISTS(SELECT 1 FROM native_work_legacy_fts_data WHERE id>10)`,
			`DELETE FROM native_work_legacy_fts_data WHERE id IN(SELECT id FROM native_work_legacy_fts_data WHERE id>10 ORDER BY id LIMIT ?)`},
		{`SELECT EXISTS(SELECT 1 FROM native_work_legacy_fts_idx)`,
			`DELETE FROM native_work_legacy_fts_idx WHERE (segid,term) IN(SELECT segid,term FROM native_work_legacy_fts_idx ORDER BY segid,term LIMIT ?)`},
		{`SELECT EXISTS(SELECT 1 FROM native_work_legacy_fts_docsize)`,
			`DELETE FROM native_work_legacy_fts_docsize WHERE id IN(SELECT id FROM native_work_legacy_fts_docsize ORDER BY id LIMIT ?)`},
	} {
		var pending bool
		if err := d.reader.QueryRowContext(ctx, batch.pending).Scan(&pending); err != nil {
			return false, fmt.Errorf("read legacy search storage: %w", err)
		}
		if !pending {
			continue
		}
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, batch.remove, nativeSearchLegacyPruneBatch); err != nil {
			return false, fmt.Errorf("retire legacy search storage: %w", err)
		}
		return true, d.commit(ctx, tx)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DROP TABLE native_work_legacy_fts`); err != nil {
		return false, fmt.Errorf("drop retired search index: %w", err)
	}
	return true, d.commit(ctx, tx)
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
				return d.retireLegacyNativeSearch(ctx)
			}
			err = d.reader.QueryRowContext(ctx, `SELECT min(id) FROM (
 SELECT (SELECT id FROM native_work_documents WHERE id>? AND id<=? ORDER BY id LIMIT 1) AS id
 UNION ALL SELECT (SELECT document_id FROM native_work_search_lengths WHERE document_id>? AND document_id<=? ORDER BY document_id LIMIT 1)
 UNION ALL SELECT (SELECT document_id FROM native_work_search_postings WHERE document_id>? AND document_id<=? ORDER BY document_id LIMIT 1)
 ) HAVING min(id) IS NOT NULL`, through, last, through, last, through, last).Scan(&next)
		} else {
			err = d.reader.QueryRowContext(ctx, `SELECT id FROM native_work_documents WHERE issue_id=? AND id>? ORDER BY id LIMIT 1`, issue, through).Scan(&next)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("select search document: %w", err)
		}
	}
	var document nativeSearchDocument
	if next != 0 {
		postings, err := d.obsoleteNativeSearchPostings(ctx, next)
		if err != nil {
			return false, err
		}
		if len(postings) != 0 {
			tx, err := d.db.BeginTx(ctx, nil)
			if err != nil {
				return false, err
			}
			defer tx.Rollback()
			if err := pruneNativeSearchPostings(ctx, tx, next, postings); err != nil {
				return false, err
			}
			return true, d.commit(ctx, tx)
		}
		document, err = d.prepareNativeSearchDocument(ctx, next)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("prepare search document: %w", err)
		}
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if next != 0 && document.issue != 0 {
		published, err := d.publishNativeSearchDocument(ctx, tx, document)
		if err != nil {
			return false, err
		}
		if !published {
			return true, d.commit(ctx, tx)
		}
	} else if next != 0 {
		for _, batch := range []nativeSearchPostingBatch{
			{`DELETE FROM native_work_search_lengths WHERE document_id=? AND NOT ` + nativeSearchDocumentSourceExists, []any{next, next}},
			{`DELETE FROM native_work_search_refresh WHERE document_id=? AND NOT ` + nativeSearchDocumentSourceExists, []any{next, next}},
			{`DELETE FROM native_work_scoped_fts WHERE rowid IN(?*2,?*2+1) AND NOT ` + nativeSearchDocumentSourceExists, []any{next, next, next}},
			{`DELETE FROM native_work_fts WHERE rowid=? AND NOT ` + nativeSearchDocumentSourceExists, []any{next, next}},
			{`DELETE FROM native_work_documents WHERE id=? AND NOT ` + nativeSearchDocumentSourceExists, []any{next, next}},
		} {
			if _, err := tx.ExecContext(ctx, batch.statement, batch.args...); err != nil {
				return false, fmt.Errorf("prune orphaned search document: %w", err)
			}
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
