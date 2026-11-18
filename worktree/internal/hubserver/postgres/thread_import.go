package postgres

import (
	"context"
	"reflect"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Repository) ImportTeamThread(ctx context.Context, organization tracker.OrganizationID, source threadstore.Import, details attachment.DetailStore) error {
	t := source.Thread
	if organization == "" || t.ID == "" || t.Creator == "" || t.RequestKey == "" || len(t.InputHash) != 64 || !validThreadTime(t.CreatedAt) || !validThreadTime(t.UpdatedAt) || t.PostCount < 0 || len(source.Posts) > 200 || len(source.Reads) > 200 || len(t.Participants) > 200 || len(t.References) > 20 {
		return corestore.ErrInvalidPage
	}
	content := threadstore.NewContent(details)
	source.Posts = append([]threadstore.PostRecord{}, source.Posts...)
	for i, post := range source.Posts {
		prepared, err := content.PreparePost(ctx, organization, post)
		if err != nil {
			return err
		}
		source.Posts[i] = prepared
	}
	if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		_, err := s.db.Exec(ctx, `INSERT INTO team_threads(organization_id,id,scope,project_id,work_item_id,title,status,creator_principal,creator_name,creator_client,request_key,input_hash,post_count,created_at,updated_at) VALUES($1,$2,$3,nullif($4,''),nullif($5,''),$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(organization_id,id) DO NOTHING`, organization, t.ID, t.Scope, t.ProjectID, t.IssueID, t.Title, t.Status, t.Creator, t.CreatorName, t.CreatorClient, t.RequestKey, t.InputHash, t.PostCount, t.CreatedAt, t.UpdatedAt)
		if err != nil {
			return mapError(ctx, err)
		}
		current, err := s.ThreadByRequest(ctx, t.Creator, t.RequestKey)
		if err != nil {
			return err
		}
		expected := t
		expected.Participants = []threadstore.Participant{}
		expected.References = []threadstore.Reference{}
		expected.Unread = 0
		expected.Preview = ""
		expected.IssueNumber = 0
		current.Unread = 0
		if !reflect.DeepEqual(current, expected) {
			return corestore.ErrRevisionConflict
		}
		return s.importThreadReferences(ctx, t.ID, 0, t.References)
	}); err != nil {
		return err
	}
	for _, p := range source.Posts {
		if p.Sequence < 1 || p.Sequence > t.PostCount || p.Author == "" || p.RequestKey == "" || len(p.InputHash) != 64 || !validThreadTime(p.CreatedAt) || len(p.References) > 20 || p.BodyReference.Validate(string(organization)) != nil {
			return corestore.ErrInvalidPage
		}
		if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			archive, err := s.registerThreadBody(ctx, p)
			if err != nil {
				return err
			}
			p.BodyArchiveID = archive.ID
			_, err = s.db.Exec(ctx, `INSERT INTO team_thread_posts(organization_id,thread_id,seq,author_principal,author_name,acting_for,client_name,kind,request_key,input_hash,created_at,body_archive_id,body_object_key,body_sha256,body_bytes,body_content_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT(organization_id,thread_id,seq) DO NOTHING`, organization, t.ID, p.Sequence, p.Author, p.Name, p.ActingFor, p.Client, p.Kind, p.RequestKey, p.InputHash, p.CreatedAt, p.BodyArchiveID, p.BodyReference.Key, p.BodyReference.SHA256, p.BodyReference.CompressedSize, p.BodyReference.UncompressedSize)
			if err != nil {
				return mapError(ctx, err)
			}
			current, err := s.ThreadPostByRequest(ctx, t.ID, p.Author, p.RequestKey)
			if err != nil {
				return err
			}
			expected := p
			expected.Body = ""
			expected.References = []threadstore.Reference{}
			if !reflect.DeepEqual(current, expected) {
				return corestore.ErrRevisionConflict
			}
			if err := s.ownThreadBody(ctx, t.ID, p); err != nil {
				return err
			}
			return s.importThreadReferences(ctx, t.ID, p.Sequence, p.References)
		}); err != nil {
			return err
		}
	}
	for _, participant := range t.Participants {
		if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			_, err := s.db.Exec(ctx, `INSERT INTO team_thread_participants(organization_id,thread_id,principal_id,client_name,display_name) VALUES($1,$2,$3,$4,$5) ON CONFLICT(organization_id,thread_id,principal_id,client_name) DO NOTHING`, organization, t.ID, participant.PrincipalID, participant.Client, participant.Name)
			if err != nil {
				return mapError(ctx, err)
			}
			var name string
			err = s.db.QueryRow(ctx, `SELECT display_name FROM team_thread_participants WHERE organization_id=$1 AND thread_id=$2 AND principal_id=$3 AND client_name=$4`, organization, t.ID, participant.PrincipalID, participant.Client).Scan(&name)
			if err != nil {
				return mapError(ctx, err)
			}
			if name != participant.Name {
				return corestore.ErrRevisionConflict
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, read := range source.Reads {
		if read.Principal == "" || read.LastSequence < 0 || read.LastSequence > t.PostCount {
			return corestore.ErrInvalidPage
		}
		if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			_, err := s.db.Exec(ctx, `INSERT INTO team_thread_reads(organization_id,thread_id,principal_id,last_seq) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id,thread_id,principal_id) DO NOTHING`, organization, t.ID, read.Principal, read.LastSequence)
			if err != nil {
				return mapError(ctx, err)
			}
			current, err := s.ThreadReadCursor(ctx, t.ID, read.Principal)
			if err != nil {
				return err
			}
			if current != read.LastSequence {
				return corestore.ErrRevisionConflict
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
func (s *session) importThreadReferences(ctx context.Context, id string, seq int64, refs []threadstore.Reference) error {
	for _, ref := range refs {
		_, err := s.db.Exec(ctx, `INSERT INTO team_thread_references(organization_id,thread_id,post_seq,kind,target_id,project_id) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(organization_id,thread_id,post_seq,kind,target_id) DO NOTHING`, s.organization, id, seq, ref.Kind, ref.ID, ref.ProjectID)
		if err != nil {
			return mapError(ctx, err)
		}
		var project string
		err = s.db.QueryRow(ctx, `SELECT project_id FROM team_thread_references WHERE organization_id=$1 AND thread_id=$2 AND post_seq=$3 AND kind=$4 AND target_id=$5`, s.organization, id, seq, ref.Kind, ref.ID).Scan(&project)
		if err != nil {
			return mapError(ctx, err)
		}
		if project != ref.ProjectID {
			return corestore.ErrRevisionConflict
		}
	}
	return nil
}
