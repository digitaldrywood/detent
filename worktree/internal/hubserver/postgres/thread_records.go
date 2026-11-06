package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
)

const threadColumns = `t.id,t.scope,coalesce(t.project_id,''),coalesce(t.work_item_id,''),t.title,t.status,t.creator_principal,t.creator_name,t.creator_client,t.post_count,greatest(0,t.post_count-coalesce(r.last_seq,0)),t.created_at,t.updated_at,t.request_key,t.input_hash`
const threadVisibility = `(t.scope='general' OR (EXISTS(SELECT 1 FROM projects p WHERE p.organization_id=t.organization_id AND p.id=t.project_id AND p.deleted_at IS NULL) AND ($3 OR t.project_id=ANY($4::text[])) AND (t.scope<>'issue' OR EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=t.organization_id AND i.project_id=t.project_id AND i.native_id=t.work_item_id AND NOT i.archived AND ($5='' OR i.actor_principal_id=$5)))))`

func scanThread(row interface{ Scan(...any) error }) (threadstore.Record, error) {
	var t threadstore.Record
	err := row.Scan(&t.ID, &t.Scope, &t.ProjectID, &t.IssueID, &t.Title, &t.Status, &t.Creator, &t.CreatorName, &t.CreatorClient, &t.PostCount, &t.Unread, &t.CreatedAt, &t.UpdatedAt, &t.RequestKey, &t.InputHash)
	t.Participants = []threadstore.Participant{}
	t.References = []threadstore.Reference{}
	return t, err
}
func (s *session) threadPage(ctx context.Context, id string, f threadstore.Filter) ([]threadstore.Record, error) {
	if f.Limit < 1 || f.Limit > 51 || len(f.Projects) > 200 {
		return nil, corestore.ErrInvalidPage
	}
	projects := make([]string, len(f.Projects))
	for i, p := range f.Projects {
		projects[i] = string(p)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadColumns+` FROM team_threads t LEFT JOIN team_thread_reads r ON r.organization_id=t.organization_id AND r.thread_id=t.id AND r.principal_id=$2 WHERE t.organization_id=$1 AND `+threadVisibility+` AND ($6='' OR t.scope=$6) AND ($7='' OR t.project_id=$7) AND ($8='' OR t.work_item_id=$8) AND ($9='' OR t.status=$9) AND ($10='' OR to_tsvector('english',t.title) @@ to_tsquery('english',$10)) AND ($11='' OR (t.updated_at,t.id)<($11,$12)) AND ($14='' OR t.id=$14) ORDER BY t.updated_at DESC,t.id DESC LIMIT $13`, s.organization, f.Principal, f.AllProjects, pq.Array(projects), f.Reporter, f.Scope, f.ProjectID, f.IssueID, f.Status, f.Search, f.BeforeTime, f.BeforeID, f.Limit, id)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []threadstore.Record{}
	for rows.Next() {
		row, err := scanThread(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		out = append(out, row)
	}
	return out, mapError(ctx, rows.Err())
}
func (s *session) Threads(ctx context.Context, f threadstore.Filter) ([]threadstore.Record, error) {
	return s.threadPage(ctx, "", f)
}
func (s *session) Thread(ctx context.Context, id string, f threadstore.Filter) (threadstore.Record, error) {
	f.Limit = 1
	rows, err := s.threadPage(ctx, id, f)
	if err != nil {
		return threadstore.Record{}, err
	}
	if len(rows) == 0 {
		return threadstore.Record{}, corestore.ErrNotFound
	}
	return rows[0], nil
}
func (s *session) ThreadByRequest(ctx context.Context, principal, key string) (threadstore.Record, error) {
	row, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM team_threads t LEFT JOIN team_thread_reads r ON r.organization_id=t.organization_id AND r.thread_id=t.id AND r.principal_id=$2 WHERE t.organization_id=$1 AND t.creator_principal=$2 AND t.request_key=$3`, s.organization, principal, key))
	return row, mapError(ctx, err)
}
func (s *session) ThreadParticipants(ctx context.Context, id string) ([]threadstore.Participant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT principal_id,display_name,client_name FROM team_thread_participants WHERE organization_id=$1 AND thread_id=$2 ORDER BY principal_id,client_name LIMIT 20`, s.organization, id)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []threadstore.Participant{}
	for rows.Next() {
		var p threadstore.Participant
		if err := rows.Scan(&p.PrincipalID, &p.Name, &p.Client); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, p)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) ThreadReferences(ctx context.Context, id string, seq int64) ([]threadstore.Reference, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind,target_id,project_id FROM team_thread_references WHERE organization_id=$1 AND thread_id=$2 AND post_seq=$3 ORDER BY kind,target_id LIMIT 20`, s.organization, id, seq)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []threadstore.Reference{}
	for rows.Next() {
		var ref threadstore.Reference
		if err := rows.Scan(&ref.Kind, &ref.ID, &ref.ProjectID); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, ref)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) ThreadReferenceTarget(ctx context.Context, kind, id string) (threadstore.Target, error) {
	target := threadstore.Target{Reference: threadstore.Reference{Kind: kind, ID: id}}
	var err error
	switch kind {
	case "issue":
		var number int64
		err = s.db.QueryRowContext(ctx, `SELECT project_id,number FROM issues WHERE organization_id=$1 AND native_id=$2 AND NOT archived`, s.organization, id).Scan(&target.ProjectID, &number)
		target.Label = "#" + strconv.FormatInt(number, 10)
	case "change":
		err = s.db.QueryRowContext(ctx, `SELECT project_id,work_item_id FROM change_requests WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(&target.ProjectID, &target.IssueID)
	case "attachment":
		err = s.db.QueryRowContext(ctx, `SELECT project_id,name,uploader FROM attachments WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL`, s.organization, id).Scan(&target.ProjectID, &target.Label, &target.Uploader)
	default:
		return target, corestore.ErrInvalidPage
	}
	return target, mapError(ctx, err)
}
func scanThreadPost(row interface{ Scan(...any) error }) (threadstore.PostRecord, error) {
	var p threadstore.PostRecord
	err := row.Scan(&p.Sequence, &p.Author, &p.Name, &p.ActingFor, &p.Client, &p.Kind, &p.CreatedAt, &p.RequestKey, &p.InputHash, &p.BodyArchiveID, &p.BodyReference.Key, &p.BodyReference.SHA256, &p.BodyReference.CompressedSize, &p.BodyReference.UncompressedSize)
	p.BodyReference = eventstore.Reference(p.BodyReference.Key, p.BodyReference.SHA256, p.BodyReference.CompressedSize, p.BodyReference.UncompressedSize)
	p.References = []threadstore.Reference{}
	return p, err
}

const threadPostColumns = `seq,author_principal,author_name,acting_for,client_name,kind,created_at,request_key,input_hash,body_archive_id,body_object_key,body_sha256,body_bytes,body_content_bytes`

func (s *session) ThreadPosts(ctx context.Context, id string, after, through int64, limit int) ([]threadstore.PostRecord, error) {
	if limit < 1 || limit > 50 || after < 0 || through < after {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadPostColumns+` FROM team_thread_posts WHERE organization_id=$1 AND thread_id=$2 AND seq>$3 AND seq<=$4 ORDER BY seq LIMIT $5`, s.organization, id, after, through, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []threadstore.PostRecord{}
	for rows.Next() {
		p, err := scanThreadPost(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, p)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) ThreadPostByRequest(ctx context.Context, id, principal, key string) (threadstore.PostRecord, error) {
	p, err := scanThreadPost(s.db.QueryRowContext(ctx, `SELECT `+threadPostColumns+` FROM team_thread_posts WHERE organization_id=$1 AND thread_id=$2 AND author_principal=$3 AND request_key=$4`, s.organization, id, principal, key))
	return p, mapError(ctx, err)
}
func (s *session) ThreadReadCursor(ctx context.Context, id, principal string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT last_seq FROM team_thread_reads WHERE organization_id=$1 AND thread_id=$2 AND principal_id=$3`, s.organization, id, principal).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, mapError(ctx, err)
}
func validThreadTime(raw string) bool {
	at, err := time.Parse(time.RFC3339Nano, raw)
	return err == nil && at.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") == raw
}
func (s *session) CreateThread(ctx context.Context, t threadstore.Record) error {
	if t.ID == "" || t.Creator == "" || t.RequestKey == "" || len(t.RequestKey) > 200 || len(t.InputHash) != 64 || strings.TrimSpace(t.Title) == "" || utf8.RuneCountInString(t.Title) > 200 || !validThreadTime(t.CreatedAt) || !validThreadTime(t.UpdatedAt) || len(t.References) > 20 {
		return corestore.ErrInvalidPage
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO team_threads(organization_id,id,scope,project_id,work_item_id,title,status,creator_principal,creator_name,creator_client,request_key,input_hash,created_at,updated_at) VALUES($1,$2,$3,nullif($4,''),nullif($5,''),$6,'open',$7,$8,$9,$10,$11,$12,$13)`, s.organization, t.ID, t.Scope, t.ProjectID, t.IssueID, t.Title, t.Creator, t.CreatorName, t.CreatorClient, t.RequestKey, t.InputHash, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	return s.insertThreadReferences(ctx, t.ID, 0, t.References)
}
func (s *session) insertThreadReferences(ctx context.Context, id string, seq int64, refs []threadstore.Reference) error {
	if len(refs) > 20 {
		return corestore.ErrInvalidPage
	}
	for _, ref := range refs {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO team_thread_references(organization_id,thread_id,post_seq,kind,target_id,project_id) VALUES($1,$2,$3,$4,$5,$6)`, s.organization, id, seq, ref.Kind, ref.ID, ref.ProjectID); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}
func (s *session) AppendThreadPost(ctx context.Context, id string, p threadstore.PostRecord) (threadstore.PostRecord, error) {
	if p.Author == "" || p.RequestKey == "" || len(p.RequestKey) > 200 || len(p.InputHash) != 64 || strings.TrimSpace(p.Body) == "" || len(p.Body) > 8192 || !validThreadTime(p.CreatedAt) || len(p.References) > 20 || p.BodyReference.Validate(string(s.organization)) != nil {
		return p, corestore.ErrInvalidPage
	}
	var locked string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM team_threads WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, id).Scan(&locked); err != nil {
		return p, mapError(ctx, err)
	}
	existing, err := s.ThreadPostByRequest(ctx, id, p.Author, p.RequestKey)
	if err == nil {
		if existing.InputHash != p.InputHash {
			return p, threadstore.ErrIdempotencyConflict
		}
		existing.References, err = s.ThreadReferences(ctx, id, existing.Sequence)
		return existing, err
	}
	if !errors.Is(err, corestore.ErrNotFound) {
		return p, err
	}

	archive, err := s.registerThreadBody(ctx, p)
	if err != nil {
		return p, err
	}
	p.BodyArchiveID = archive.ID
	if err := s.db.QueryRowContext(ctx, `UPDATE team_threads SET post_count=post_count+1,updated_at=$3 WHERE organization_id=$1 AND id=$2 RETURNING post_count`, s.organization, id, p.CreatedAt).Scan(&p.Sequence); err != nil {
		return p, mapError(ctx, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO team_thread_posts(organization_id,thread_id,seq,author_principal,author_name,acting_for,client_name,kind,request_key,input_hash,created_at,body_archive_id,body_object_key,body_sha256,body_bytes,body_content_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, s.organization, id, p.Sequence, p.Author, p.Name, p.ActingFor, p.Client, p.Kind, p.RequestKey, p.InputHash, p.CreatedAt, p.BodyArchiveID, p.BodyReference.Key, p.BodyReference.SHA256, p.BodyReference.CompressedSize, p.BodyReference.UncompressedSize)
	if err != nil {
		return p, mapError(ctx, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO team_thread_participants(organization_id,thread_id,principal_id,client_name,display_name) VALUES($1,$2,$3,$4,$5) ON CONFLICT(organization_id,thread_id,principal_id,client_name) DO UPDATE SET display_name=excluded.display_name`, s.organization, id, p.Author, p.Client, p.Name)
	if err != nil {
		return p, mapError(ctx, err)
	}
	if err := s.ownThreadBody(ctx, id, p); err != nil {
		return p, err
	}
	return p, s.insertThreadReferences(ctx, id, p.Sequence, p.References)
}
func (s *session) ResolveThread(ctx context.Context, id, status string) error {
	if status != "open" && status != "resolved" {
		return corestore.ErrInvalidPage
	}
	result, err := s.db.ExecContext(ctx, `UPDATE team_threads SET status=$3 WHERE organization_id=$1 AND id=$2`, s.organization, id, status)
	return updateOwned(ctx, result, err)
}
func (s *session) AdvanceThreadRead(ctx context.Context, id, principal string, seq int64) error {
	if principal == "" || seq < 0 {
		return corestore.ErrInvalidPage
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO team_thread_reads(organization_id,thread_id,principal_id,last_seq) SELECT $1,$2,$3,$4 WHERE EXISTS(SELECT 1 FROM team_threads WHERE organization_id=$1 AND id=$2 AND post_count>=$4) ON CONFLICT(organization_id,thread_id,principal_id) DO UPDATE SET last_seq=greatest(team_thread_reads.last_seq,excluded.last_seq)`, s.organization, id, principal, seq)
	return mapError(ctx, err)
}

func (s *session) registerThreadBody(ctx context.Context, post threadstore.PostRecord) (artifactstore.ArchiveObject, error) {
	created, err := time.Parse(time.RFC3339Nano, post.CreatedAt)
	if err != nil {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	id := "att_" + artifact.Digest([]byte(string(s.organization) + "\n" + post.BodyReference.Key))[:32]
	return s.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: s.organization, ID: id, Reference: post.BodyReference, CreatedAt: created})
}
func (s *session) ownThreadBody(ctx context.Context, id string, post threadstore.PostRecord) error {
	created, err := time.Parse(time.RFC3339Nano, post.CreatedAt)
	if err != nil {
		return corestore.ErrInvalidPage
	}
	return s.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: s.organization, ArchiveID: post.BodyArchiveID, Kind: "team-thread-post", ID: id + "/" + strconv.FormatInt(post.Sequence, 10), CreatedAt: created})
}
