package postgres

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ searchstore.Repository = (*Repository)(nil)

func (r *Repository) ReadSearch(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(searchstore.Reader) error) error {
	return r.transaction(ctx, organization, workload, true, func(s *session) error { return read(s) })
}

func (r *Repository) WriteSearch(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(searchstore.Writer) error) error {
	return r.transaction(ctx, organization, workload, false, func(s *session) error { return write(s) })
}

func (s *session) PendingSearch(ctx context.Context) (searchstore.Document, error) {
	transaction := s.transactionHandle()
	row, err := transaction.Queries.SearchPending(ctx, transaction.Organization)
	return searchDocument(row), mapError(ctx, err)
}

func searchDocument(row pgquery.SearchDocument) searchstore.Document {
	d := searchstore.Document{Generation: row.Generation, Placeholder: row.Placeholder, OrganizationID: row.OrganizationID, ID: row.ID, Kind: row.Kind, SourceID: row.SourceID, Revision: row.Revision, Title: row.Title, Scope: row.Scope, Status: row.Status, UpdatedAt: row.UpdatedAt.Add(time.Duration(row.UpdatedAtSubmicrosecond)),
		Payload: attachment.ArchiveReference{Key: row.PayloadObjectKey, SHA256: row.PayloadSHA256, CompressedSize: row.PayloadBytes, UncompressedSize: row.PayloadContentBytes, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}}
	if row.ProjectID != nil {
		d.ProjectID = *row.ProjectID
	}
	if row.WorkItemID != nil {
		d.WorkItemID = tracker.NativeWorkItemID(*row.WorkItemID)
	}
	return d
}

func (s *session) SearchSources(ctx context.Context, d searchstore.Document, after string, limit int) ([]searchstore.Document, error) {
	transaction := s.transactionHandle()
	if err := transaction.CheckOrganization(d.OrganizationID); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 32 {
		return nil, searchstore.ErrInvalid
	}
	rows, err := transaction.Queries.SearchSources(ctx, pgquery.SearchSourcesParams{OrganizationID: transaction.Organization, ProjectID: new(d.ProjectID), WorkItemID: new(string(d.WorkItemID)), AfterID: after, PageSize: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	documents := make([]searchstore.Document, len(rows))
	for i, row := range rows {
		documents[i] = searchDocument(row)
	}
	return documents, nil
}

func (s *session) AdvanceSearch(ctx context.Context, d searchstore.Document) error {
	transaction := s.transactionHandle()
	if err := transaction.CheckOrganization(d.OrganizationID); err != nil {
		return err
	}
	return mapError(ctx, transaction.Queries.SearchAdvance(ctx, pgquery.SearchAdvanceParams{OrganizationID: transaction.Organization, ID: d.ID, Revision: d.Revision}))
}

func (s *session) PutSearch(ctx context.Context, d searchstore.Document, expected int64) error {
	transaction := s.transactionHandle()
	if err := transaction.CheckOrganization(d.OrganizationID); err != nil {
		return err
	}
	if d.Validate() != nil || expected < 0 || d.Revision <= expected || d.Payload.Validate(string(transaction.Organization)) != nil {
		return searchstore.ErrInvalid
	}
	source := d.Source
	if source == nil || source.Statistics.Validate() != nil {
		return searchstore.ErrInvalid
	}
	if err := s.lockDuplicateSourceProject(ctx, d.ProjectID, d.Kind); err != nil {
		return err
	}
	var project *tracker.ProjectID
	var item *string
	if d.ProjectID != "" {
		project = new(d.ProjectID)
	}
	if d.WorkItemID != "" {
		item = new(string(d.WorkItemID))
	}
	if expected > 0 {
		count, err := transaction.Queries.SearchUpdate(ctx, pgquery.SearchUpdateParams{OrganizationID: transaction.Organization, ID: d.ID, ProjectID: project, WorkItemID: item, Kind: d.Kind, SourceID: d.SourceID, Revision: d.Revision, ExpectedRevision: expected, Title: d.Title, Scope: d.Scope, Status: d.Status, UpdatedAt: d.UpdatedAt.Truncate(time.Microsecond), UpdatedAtSubmicrosecond: int64(d.UpdatedAt.Nanosecond() % 1000), PayloadObjectKey: d.Payload.Key, PayloadSHA256: d.Payload.SHA256, PayloadBytes: d.Payload.CompressedSize, PayloadContentBytes: d.Payload.UncompressedSize, SourceVector: source.Vector, TokenCount: source.Statistics.Tokens, Terms: source.Statistics.Terms, TermFrequencies: source.Statistics.Frequencies})
		if err != nil || count > 0 {
			return affected(ctx, count, err)
		}
		expected = 0
	}
	if d.Kind == "issue" {
		if err := transaction.Queries.SearchRemovePlaceholder(ctx, pgquery.SearchRemovePlaceholderParams{OrganizationID: transaction.Organization, ProjectID: project, SourceID: d.SourceID, ID: d.ID}); err != nil {
			return mapError(ctx, err)
		}
	}

	count, err := transaction.Queries.SearchPut(ctx, pgquery.SearchPutParams{OrganizationID: transaction.Organization, ID: d.ID, ProjectID: project, WorkItemID: item, Kind: d.Kind, SourceID: d.SourceID, Revision: d.Revision, ExpectedRevision: expected, Title: d.Title, Scope: d.Scope, Status: d.Status, UpdatedAt: d.UpdatedAt.Truncate(time.Microsecond), UpdatedAtSubmicrosecond: int64(d.UpdatedAt.Nanosecond() % 1000), PayloadObjectKey: d.Payload.Key, PayloadSHA256: d.Payload.SHA256, PayloadBytes: d.Payload.CompressedSize, PayloadContentBytes: d.Payload.UncompressedSize, SourceVector: source.Vector, TokenCount: source.Statistics.Tokens, Terms: source.Statistics.Terms, TermFrequencies: source.Statistics.Frequencies})
	return affected(ctx, count, err)
}

func (s *session) DeleteSearch(ctx context.Context, id string, revision int64) error {
	transaction := s.transactionHandle()
	if err := s.lockDuplicateSourceDocument(ctx, id); err != nil {
		return err
	}
	count, err := transaction.Queries.SearchDelete(ctx, pgquery.SearchDeleteParams{OrganizationID: transaction.Organization, ID: id, Revision: revision})
	return affected(ctx, count, err)
}

func (s *session) PublishSearch(ctx context.Context, value searchstore.Prepared) (bool, error) {
	transaction := s.transactionHandle()
	d := value.Document
	if err := transaction.CheckOrganization(d.OrganizationID); err != nil {
		return false, err
	}
	if err := value.Statistics.Validate(); err != nil {
		return false, err
	}
	params := pgquery.SearchPublishParams{Generation: d.Generation, OrganizationID: transaction.Organization, ID: d.ID, Revision: d.Revision, PayloadObjectKey: d.Payload.Key, PayloadSHA256: d.Payload.SHA256, Vector: value.Vector, TokenCount: value.Statistics.Tokens, Terms: value.Statistics.Terms, TermFrequencies: value.Statistics.Frequencies}
	if value.Payload.Key != "" {
		if err := value.Payload.Validate(string(transaction.Organization)); err != nil {
			return false, err
		}
		params.AggregateObjectKey = new(value.Payload.Key)
		params.AggregateSha256 = new(value.Payload.SHA256)
		params.AggregateBytes = pgtype.Int8{Int64: value.Payload.CompressedSize, Valid: true}
		params.AggregateContentBytes = pgtype.Int8{Int64: value.Payload.UncompressedSize, Valid: true}
	}
	prefix := searchstore.Prefix(d.OrganizationID, d.ProjectID)
	terms := make([]string, 0, len(value.Literals))
	positions := make([]string, 0, len(value.Literals))
	for _, posting := range value.Literals {
		if d.Kind == "issue" && !strings.HasPrefix(posting.Term, prefix) {
			continue
		}
		terms = append(terms, posting.Term)
		positions = append(positions, literalPositionsArray(posting.Positions))
	}
	if _, err := transaction.DB.Exec(ctx, "SAVEPOINT search_publication"); err != nil {
		return false, mapError(ctx, err)
	}
	discard := func() (bool, error) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := transaction.DB.Exec(cleanup, "ROLLBACK TO SAVEPOINT search_publication"); err != nil {
			return false, mapError(cleanup, err)
		}
		_, err := transaction.DB.Exec(cleanup, "RELEASE SAVEPOINT search_publication")
		return false, mapError(cleanup, err)
	}
	captured, err := transaction.Queries.SearchPreparePublicationPostings(ctx, pgquery.SearchPreparePublicationPostingsParams{OrganizationID: transaction.Organization, ID: d.ID, Revision: d.Revision, PayloadObjectKey: d.Payload.Key, PayloadSHA256: d.Payload.SHA256, Generation: d.Generation, Vector: value.Vector, TokenCount: value.Statistics.Tokens, Terms: value.Statistics.Terms, TermFrequencies: value.Statistics.Frequencies, Literals: terms})
	if errors.Is(err, pgx.ErrNoRows) {
		return discard()
	}
	if err != nil {
		return false, mapError(ctx, err)
	}
	if len(terms) > 0 {
		if err := transaction.Queries.SearchPublishLiterals(ctx, pgquery.SearchPublishLiteralsParams{OrganizationID: transaction.Organization, ID: d.ID, Revision: d.Revision, Literals: terms, Positions: positions, Scopes: captured.VisibilityScopes, PrincipalID: captured.ParentPrincipal}); err != nil {
			return false, mapError(ctx, err)
		}
	}
	current, err := transaction.Queries.SearchPublicationVisibility(ctx, pgquery.SearchPublicationVisibilityParams{OrganizationID: transaction.Organization, ID: d.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return discard()
	}
	if err != nil {
		return false, mapError(ctx, err)
	}
	if current.ProjectHidden != captured.ProjectHidden || current.ParentArchived != captured.ParentArchived || current.ParentPrincipal != captured.ParentPrincipal || current.ProjectName != captured.ProjectName || current.ParentNumber != captured.ParentNumber || current.ParentProject != captured.ParentProject {
		return discard()
	}
	count, err := transaction.Queries.SearchPublish(ctx, params)
	if err != nil {
		return false, mapError(ctx, err)
	}
	if count == 0 {
		return discard()
	}
	_, err = transaction.DB.Exec(ctx, "RELEASE SAVEPOINT search_publication")
	return true, mapError(ctx, err)
}

func literalPositionsArray(positions []int64) string {
	if len(positions) == 0 {
		return "{}"
	}
	var result strings.Builder
	result.WriteByte('{')
	for index, position := range positions {
		if index > 0 {
			result.WriteByte(',')
		}
		result.WriteString(strconv.FormatInt(position, 10))
	}
	result.WriteByte('}')
	return result.String()
}

func (s *session) Search(ctx context.Context, query searchstore.Query) ([]searchstore.Hit, error) {
	transaction := s.transactionHandle()
	if err := query.Validate(); err != nil {
		return nil, err
	}
	expressions := searchstore.Expressions(transaction.Organization, query)
	ranking := strings.Join(expressions, " | ")
	expression := ranking
	if query.Kind == "work" && query.Text != "" && !query.Exact {
		literal := searchstore.LiteralExpression(transaction.Organization, query)
		if literal != "" {
			if expression != "" {
				expression += " | "
			}
			expression += "(" + literal + ")"
		}
	}
	if query.Kind == "thread" && query.Text == "" {
		expression = ""
	}
	projectIDs := make([]string, len(query.ProjectIDs))
	for i, id := range query.ProjectIDs {
		projectIDs[i] = string(id)
	}
	principal := ""
	if query.Reporter != nil {
		principal = *query.Reporter
	}
	number, name := searchstore.Number(query.Text)
	before := time.Time{}
	if query.BeforeUpdatedAt != nil {
		before = *query.BeforeUpdatedAt
	}
	keys, ids, times := []string{}, []string{}, []string{}
	impacts, submicroseconds, done := []float64{}, []int64{}, []bool{}
	for _, stream := range query.Streams {
		keys, ids = append(keys, stream.Key+"\t"+stream.Lexeme), append(ids, stream.ID)
		impacts = append(impacts, stream.Impact)
		times = append(times, stream.UpdatedAt.Truncate(time.Microsecond).Format(time.RFC3339Nano))
		submicroseconds = append(submicroseconds, int64(stream.UpdatedAt.Nanosecond()%1000))
		done = append(done, stream.Done)
	}
	rows, err := transaction.Queries.SearchCandidates(ctx, pgquery.SearchCandidatesParams{StreamKeys: keys, StreamIds: ids, StreamImpacts: impacts, StreamTimes: times, StreamSubmicroseconds: submicroseconds, StreamDone: done, OrganizationID: transaction.Organization, EmptyQuery: query.Text == "", BeforeUpdatedAt: before.Truncate(time.Microsecond), BeforeSubmicrosecond: int64(before.Nanosecond() % 1000), BeforeID: query.BeforeID, Expression: expression, SearchTerms: searchstore.CandidateTerms(transaction.Organization, query), Exact: query.Exact, ProjectID: string(query.ProjectID), WorkItemID: string(query.WorkItemID), Kind: query.Kind, Scope: query.Scope, Status: query.Status, Archived: query.Archived, Reporter: query.Reporter != nil, PrincipalID: principal, RestrictProjects: query.ProjectIDs != nil, ProjectIds: projectIDs, Number: number, NumberProject: name})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	hits := make([]searchstore.Hit, len(rows))
	for i, row := range rows {
		hits[i] = searchstore.Hit{ScanBoundary: row.ScanBoundary, Document: searchDocument(pgquery.SearchDocument{Generation: row.Generation, Placeholder: row.Placeholder, OrganizationID: row.OrganizationID, ID: row.ID, ProjectID: new(tracker.ProjectID(row.ProjectID)), WorkItemID: row.WorkItemID, Kind: row.Kind, SourceID: row.SourceID, Revision: row.Revision, Title: row.Title, Scope: row.Scope, Status: row.Status, UpdatedAt: row.UpdatedAt, UpdatedAtSubmicrosecond: row.UpdatedAtSubmicrosecond, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes}), Number: row.Number, ProjectName: row.ProjectName}
		if row.StreamKey != "" {
			key, rest, _ := strings.Cut(row.StreamKey, "\t")
			lexeme, advance, _ := strings.Cut(rest, "\t")
			hits[i].Stream = &searchstore.StreamPosition{Key: key, Lexeme: lexeme, ID: row.ID, Impact: row.StreamImpact, UpdatedAt: hits[i].Document.UpdatedAt, Done: row.StreamExhausted}
			if advance == "advance" {
				hits[i].Stream.ID = ""
			}
		}
		if row.IndexedRevision == row.Revision {
			hits[i].Index = &searchstore.Statistics{Tokens: row.TokenCount, Terms: row.Terms, Frequencies: row.TermFrequencies}
		}
	}
	return hits, nil
}

func (s *session) SearchProgress(ctx context.Context, d searchstore.Document) (searchstore.Progress, error) {
	transaction := s.transactionHandle()
	if err := transaction.CheckOrganization(d.OrganizationID); err != nil {
		return searchstore.Progress{}, err
	}
	var progress searchstore.Progress
	ref := &progress.Payload
	err := transaction.DB.QueryRow(ctx, `SELECT progress_after_id,progress_object_key,progress_sha256,progress_bytes,progress_content_bytes FROM search_documents WHERE organization_id=$1 AND id=$2 AND generation=$3 AND progress_generation=$3 AND revision=$4 AND payload_object_key=$5 AND payload_sha256=$6`, transaction.Organization, d.ID, d.Generation, d.Revision, d.Payload.Key, d.Payload.SHA256).Scan(&progress.AfterID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize)
	if errors.Is(err, pgx.ErrNoRows) {
		return searchstore.Progress{}, nil
	}
	if err != nil {
		return searchstore.Progress{}, mapError(ctx, err)
	}
	ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention = "gzip", 1, "archive", attachment.ArchiveRetention
	return progress, nil
}

func (s *session) SaveSearchProgress(ctx context.Context, d searchstore.Document, progress searchstore.Progress) (bool, error) {
	transaction := s.transactionHandle()
	if err := transaction.CheckOrganization(d.OrganizationID); err != nil {
		return false, err
	}
	if progress.AfterID == "" || progress.Payload.Validate(string(transaction.Organization)) != nil {
		return false, searchstore.ErrInvalid
	}
	ref := progress.Payload
	result, err := transaction.DB.Exec(ctx, `UPDATE search_documents SET progress_generation=$3,progress_after_id=$7,progress_object_key=$8,progress_sha256=$9,progress_bytes=$10,progress_content_bytes=$11 WHERE organization_id=$1 AND id=$2 AND generation=$3 AND revision=$4 AND payload_object_key=$5 AND payload_sha256=$6 AND (progress_generation IS DISTINCT FROM $3 OR progress_after_id COLLATE "C"<$7 COLLATE "C")`, transaction.Organization, d.ID, d.Generation, d.Revision, d.Payload.Key, d.Payload.SHA256, progress.AfterID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize)
	if err != nil {
		return false, mapError(ctx, err)
	}
	count := result.RowsAffected()
	return count == 1, mapError(ctx, err)
}
