package threads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) threadRecordFilter(ctx context.Context, r tenantstore.Reader, scope access) (threadstore.Filter, error) {
	f := threadstore.Filter{Principal: scope.caller.Principal, AllProjects: scope.caller.AllProjects}
	if scope.caller.Hosted {
		member, err := r.AuthorizeAccess(ctx, scope.caller.Authority(scope.project, scope.item), s.deps.Now())
		if err != nil {
			return f, err
		}
		if member.UserID != "" && hubapi.LesserHostedRole(member.Role, scope.caller.HostedRole) != scope.caller.HostedRole {
			return f, auth.ErrHostedIdentity
		}
	}
	if scope.caller.HostedRole == "reporter" {
		f.Reporter = scope.caller.ActorPrincipal
		if f.Reporter == "" {
			return f, operatortool.ErrAccessDenied
		}
	}
	if !f.AllProjects {
		if !scope.caller.Hosted {
			grants, err := r.AccessTokenGrants(ctx, scope.caller.CredentialID, "", 200)
			if err != nil {
				return f, err
			}
			if len(grants) > 200 {
				return f, corestore.ErrInvalidPage
			}
			for _, grant := range grants {
				f.Projects = append(f.Projects, grant.ProjectID)
			}
		} else {
			reader, ok := r.(accessstore.ProjectAccessPageReader)
			if !ok {
				return f, operatortool.ErrAccessDenied
			}
			filter := scope.caller.ProjectFilter
			filter.Limit = 200
			if !scope.caller.EntryKey {
				filter.KeyKind = ""
			}
			rows, err := reader.ProjectAccessPage(ctx, filter)
			if err != nil {
				return f, err
			}
			if len(rows) > 200 {
				return f, corestore.ErrInvalidPage
			}
			for _, row := range rows {
				f.Projects = append(f.Projects, row.ProjectID)
			}
		}
	}
	return f, nil
}
func (s *Service) threadRecordReference(ctx context.Context, r tenantstore.Reader, scope access, kind, id string) (threadstore.Reference, error) {
	target, err := r.ThreadReferenceTarget(ctx, kind, id)
	if err != nil {
		return threadstore.Reference{}, err
	}
	scope.project = tracker.ProjectID(target.ProjectID)
	if err := scope.authorize(ctx, r, false); err != nil {
		if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) {
			return threadstore.Reference{}, corestore.ErrNotFound
		}
		return threadstore.Reference{}, err
	}
	switch kind {
	case "issue":
		err = scope.requireItem(ctx, r, id)
	case "change":
		err = scope.requireItem(ctx, r, target.IssueID)
	case "attachment":
		err = scope.readAttachment(ctx, r, id)
	default:
		return threadstore.Reference{}, Invalid()
	}
	return target.Reference, err
}
func (s *Service) threadRecordReferences(ctx context.Context, r tenantstore.Reader, scope access, id string, seq int64) ([]threadstore.Reference, error) {
	refs, err := r.ThreadReferences(ctx, id, seq)
	if err != nil {
		return nil, err
	}
	out := []threadstore.Reference{}
	for _, ref := range refs {
		current, err := s.threadRecordReference(ctx, r, scope, ref.Kind, ref.ID)
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || s.notFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, current)
	}
	return out, nil
}
func (s *Service) fillThreadRecord(ctx context.Context, r tenantstore.Reader, scope access, t *threadstore.Thread) error {
	var err error
	t.Participants, err = r.ThreadParticipants(ctx, t.ID)
	if err != nil {
		return err
	}
	t.References, err = s.threadRecordReferences(ctx, r, scope, t.ID, 0)
	if err != nil {
		return err
	}
	if t.IssueID != "" {
		ref, err := s.threadRecordReference(ctx, r, scope, "issue", t.IssueID)
		if err != nil {
			return err
		}
		number, err := strconv.Atoi(strings.TrimPrefix(ref.Label, "#"))
		if err != nil {
			return err
		}
		t.IssueNumber = number
		found := false
		for _, existing := range t.References {
			found = found || existing.Kind == "issue" && existing.ID == t.IssueID
		}
		if !found {
			t.References = append([]threadstore.Reference{ref}, t.References...)
		}
	}
	return nil
}
func (s *Service) threadRecordAuthor(ctx context.Context, scope access) (string, string, string, string) {
	principal := scope.caller.Principal
	name := scope.caller.Name
	if scope.caller.Hosted {
		name = scope.caller.HostedName
	}
	if name == "" && scope.caller.Hosted {
		if err := s.deps.Repository.ReadTenant(ctx, scope.caller.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			member, err := r.AccessMember(ctx, scope.caller.HostedSubject)
			if err == nil {
				name = member.Email
			}
			return err
		}); err != nil {
			name = principal
		}
	}
	if name == "" {
		name = principal
	}
	acting, client := "", ""
	if scope.caller.Personal || scope.caller.Agent != "" {
		acting = principal
		client = scope.caller.Agent
		if client == "" {
			client = operatortool.CurrentConnection(ctx).Client
		}
		if client == "" {
			client = scope.caller.Name
		}
	}
	return principal, name, acting, client
}
func (s *Service) validateThreadRecordReferences(ctx context.Context, r tenantstore.Reader, scope access, inputs []operatortool.ThreadReference) ([]threadstore.Reference, error) {
	refs := []threadstore.Reference{}
	seen := map[string]bool{}
	for _, input := range inputs {
		ref, err := s.threadRecordReference(ctx, r, scope, input.Kind, input.ID)
		if err != nil {
			return nil, err
		}
		ref.IssueID, ref.Label = "", ""
		if !seen[ref.Kind+":"+ref.ID] {
			refs = append(refs, ref)
			seen[ref.Kind+":"+ref.ID] = true
		}
	}
	return refs, nil
}
func (s *Service) operation(ctx context.Context, scope access, name string, in operatortool.ThreadArguments) (any, error) {
	if name == "list_threads" {
		return s.listThreadRecords(ctx, scope, in)
	}
	if name == "create_thread" {
		return s.createThreadRecord(ctx, scope, in)
	}
	var thread threadstore.Thread
	var preview *threadstore.PostRecord
	err := s.deps.Repository.ReadTenant(ctx, scope.caller.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.authorize(ctx, r, false); err != nil {
			return err
		}
		f, err := s.threadRecordFilter(ctx, r, scope)
		if err != nil {
			return err
		}
		row, err := r.Thread(ctx, in.ThreadID, f)
		if err != nil {
			return err
		}
		thread = row.Thread
		if err := s.fillThreadRecord(ctx, r, scope, &thread); err != nil {
			return err
		}
		if thread.PostCount > 0 {
			posts, err := r.ThreadPosts(ctx, thread.ID, thread.PostCount-1, thread.PostCount, 1)
			if err != nil {
				return err
			}
			if len(posts) != 1 {
				return corestore.ErrNotFound
			}
			preview = &posts[0]
		}
		return nil
	})
	if err != nil {
		return nil, s.recordError(err)
	}
	if preview != nil {
		post, err := threadstore.NewContent(s.deps.Details()).ReadPost(ctx, scope.caller.Organization, *preview)
		if err != nil {
			return nil, err
		}
		thread.Preview = hubapi.BoundRunes(post.Body, 200)
	}
	scope.project = tracker.ProjectID(thread.ProjectID)
	scope.item = thread.IssueID
	required := apikey.ScopeWrite
	if name == "get_thread" {
		required = apikey.ScopeRead
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: required, Action: name, ProjectID: thread.ProjectID}); err != nil {
		return nil, err
	}
	switch name {
	case "get_thread":
		return s.getThreadRecord(ctx, scope, thread, in)
	case "post_to_thread":
		return s.postThreadRecord(ctx, scope, thread, in)
	case "resolve_thread":
		if in.Status != "open" && in.Status != "resolved" {
			return nil, Invalid()
		}
		err = s.deps.Repository.WriteTenant(ctx, scope.caller.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := scope.authorize(ctx, w, true); err != nil {
				return err
			}
			f, err := s.threadRecordFilter(ctx, w, scope)
			if err != nil {
				return err
			}
			if _, err = w.Thread(ctx, thread.ID, f); err != nil {
				return err
			}
			return w.ResolveThread(ctx, thread.ID, in.Status)
		})
		thread.Status = in.Status
		return thread, s.recordError(err)
	}
	return nil, operatortool.ErrUnknownTool
}
func (s *Service) listThreadRecords(ctx context.Context, scope access, in operatortool.ThreadArguments) (threadstore.Page, error) {
	out := threadstore.Page{Threads: []threadstore.Thread{}}
	var previews []threadstore.PostRecord
	var previewIndexes []int
	if in.Scope != "" && in.Scope != "general" && in.Scope != "project" && in.Scope != "issue" || in.Status != "" && in.Status != "open" && in.Status != "resolved" {
		return out, Invalid()
	}
	err := s.deps.Repository.ReadTenant(ctx, scope.caller.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.authorize(ctx, r, false); err != nil {
			return err
		}
		f, err := s.threadRecordFilter(ctx, r, scope)
		if err != nil {
			return err
		}
		f.Scope, f.ProjectID, f.IssueID, f.Status = in.Scope, tracker.ProjectID(in.ProjectID), in.IssueID, in.Status
		if in.Search != "" {
			terms := strings.FieldsFunc(in.Search, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
			if len(terms) > hubapi.SearchTermLimit {
				terms = terms[:hubapi.SearchTermLimit]
			}
			for i, term := range terms {
				terms[i] = "'" + strings.ToLower(term) + "':*"
			}
			f.Search = strings.Join(terms, " | ")
			if f.Search == "" {
				return nil
			}
		}
		if in.Cursor != "" {
			var cursor []string
			raw, err := base64.RawURLEncoding.DecodeString(in.Cursor)
			if err != nil || json.Unmarshal(raw, &cursor) != nil || len(cursor) != 2 {
				return Invalid()
			}
			f.BeforeTime, f.BeforeID = cursor[0], cursor[1]
		}
		limit := Limit(in.Limit)
		f.Limit = limit + 1
		rows, err := r.Threads(ctx, f)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			rows = rows[:limit]
			last := rows[limit-1]
			raw, err := json.Marshal([]string{last.UpdatedAt, last.ID})
			if err != nil {
				return err
			}
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		}
		for _, row := range rows {
			thread := row.Thread
			if err := s.fillThreadRecord(ctx, r, scope, &thread); err != nil {
				return err
			}
			if thread.PostCount > 0 {
				posts, err := r.ThreadPosts(ctx, thread.ID, thread.PostCount-1, thread.PostCount, 1)
				if err != nil {
					return err
				}
				if len(posts) != 1 {
					return corestore.ErrNotFound
				}
				previews = append(previews, posts[0])
				previewIndexes = append(previewIndexes, len(out.Threads))
			}
			out.Threads = append(out.Threads, thread)
		}
		return nil
	})
	if err != nil {
		return out, s.recordError(err)
	}
	if err := s.HydratePosts(ctx, scope.caller.Organization, previews); err != nil {
		return out, err
	}
	for i, post := range previews {
		out.Threads[previewIndexes[i]].Preview = hubapi.BoundRunes(post.Body, 200)
	}
	err = s.deps.Repository.ReadTenant(ctx, scope.caller.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.authorize(ctx, r, false); err != nil {
			return err
		}
		filter, err := s.threadRecordFilter(ctx, r, scope)
		if err != nil {
			return err
		}
		for i := range out.Threads {
			current, err := r.Thread(ctx, out.Threads[i].ID, filter)
			if err != nil {
				return err
			}
			thread := current.Thread
			if err := s.fillThreadRecord(ctx, r, scope, &thread); err != nil {
				return err
			}
			thread.Preview = out.Threads[i].Preview
			out.Threads[i] = thread
		}
		return nil
	})
	if err != nil {
		return threadstore.Page{}, s.recordError(err)
	}
	return out, nil
}
func (s *Service) createThreadRecord(ctx context.Context, scope access, in operatortool.ThreadArguments) (threadstore.Thread, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || utf8.RuneCountInString(in.Title) > 200 {
		return threadstore.Thread{}, Invalid()
	}
	if in.Key == "" {
		in.Key = hubapi.NewID("intent")
	}
	principal, name, _, client := s.threadRecordAuthor(ctx, scope)
	now := hubapi.ConversationTime(s.deps.Now())
	record := threadstore.Record{Thread: threadstore.Thread{ID: hubapi.NewID("th"), Scope: in.Scope, ProjectID: in.ProjectID, IssueID: in.IssueID, Title: in.Title, Status: "open", Creator: principal, CreatorName: name, CreatorClient: client, CreatedAt: now, UpdatedAt: now, Participants: []threadstore.Participant{}, References: []threadstore.Reference{}}, RequestKey: in.Key}
	err := s.deps.Repository.WriteTenant(ctx, scope.caller.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		switch in.Scope {
		case "general":
			if in.ProjectID != "" || in.IssueID != "" {
				return Invalid()
			}
		case "project":
			if in.ProjectID == "" || in.IssueID != "" {
				return Invalid()
			}
		case "issue":
			target, err := w.ThreadReferenceTarget(ctx, "issue", in.IssueID)
			if err != nil {
				return err
			}
			record.ProjectID = target.ProjectID
		case "":
			return Invalid()
		default:
			return Invalid()
		}
		scope.project = tracker.ProjectID(record.ProjectID)
		scope.item = record.IssueID
		if err := scope.authorize(ctx, w, true); err != nil {
			return err
		}
		if record.IssueID != "" {
			if err := scope.requireItem(ctx, w, record.IssueID); err != nil {
				return err
			}
		}
		var err error
		record.References, err = s.validateThreadRecordReferences(ctx, w, scope, in.References)
		if err != nil {
			return err
		}
		record.InputHash, err = hubapi.CommandHash(struct {
			Scope, ProjectID, IssueID, Title string
			References                       []threadstore.Reference
		}{in.Scope, record.ProjectID, in.IssueID, in.Title, record.References})
		if err != nil {
			return err
		}
		existing, err := w.ThreadByRequest(ctx, principal, in.Key)
		if err == nil {
			if existing.InputHash != record.InputHash {
				return threadstore.ErrIdempotencyConflict
			}
			f, err := s.threadRecordFilter(ctx, w, scope)
			if err != nil {
				return err
			}
			record, err = w.Thread(ctx, existing.ID, f)
			if err != nil {
				return err
			}
			return s.fillThreadRecord(ctx, w, scope, &record.Thread)
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return w.CreateThread(ctx, record)
	})
	if errors.Is(err, corestore.ErrUniqueViolation) {
		err = s.deps.Repository.ReadTenant(ctx, scope.caller.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := scope.authorize(ctx, r, true); err != nil {
				return err
			}
			existing, err := r.ThreadByRequest(ctx, principal, in.Key)
			if err != nil {
				return err
			}
			if existing.InputHash != record.InputHash {
				return threadstore.ErrIdempotencyConflict
			}
			f, err := s.threadRecordFilter(ctx, r, scope)
			if err != nil {
				return err
			}
			record, err = r.Thread(ctx, existing.ID, f)
			if err != nil {
				return err
			}
			return s.fillThreadRecord(ctx, r, scope, &record.Thread)
		})
	}
	if err != nil {
		return threadstore.Thread{}, s.recordError(err)
	}
	if record.PostCount > 0 {
		var posts []threadstore.PostRecord
		err = s.deps.Repository.ReadTenant(ctx, scope.caller.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := scope.authorize(ctx, r, true); err != nil {
				return err
			}
			var err error
			posts, err = r.ThreadPosts(ctx, record.ID, record.PostCount-1, record.PostCount, 1)
			return err
		})
		if err != nil {
			return threadstore.Thread{}, s.recordError(err)
		}
		if len(posts) != 1 {
			return threadstore.Thread{}, hubapi.NotFound()
		}
		if err := s.HydratePosts(ctx, scope.caller.Organization, posts); err != nil {
			return threadstore.Thread{}, err
		}
		record.Preview = hubapi.BoundRunes(posts[0].Body, 200)
	}
	return record.Thread, nil
}
func (s *Service) postThreadRecord(ctx context.Context, scope access, thread threadstore.Thread, in operatortool.ThreadArguments) (threadstore.Post, error) {
	if strings.TrimSpace(in.Body) == "" || len(in.Body) > 8192 || in.Kind != "note" && in.Kind != "finding" && in.Kind != "question" && in.Kind != "decision" {
		return threadstore.Post{}, Invalid()
	}
	if in.Key == "" {
		in.Key = hubapi.NewID("intent")
	}
	principal, name, acting, client := s.threadRecordAuthor(ctx, scope)
	post := threadstore.PostRecord{Post: threadstore.Post{Author: principal, Name: name, ActingFor: acting, Client: client, Kind: in.Kind, Body: in.Body, CreatedAt: hubapi.ConversationTime(s.deps.Now())}, RequestKey: in.Key}
	content := threadstore.NewContent(s.deps.Details())
	var prepareErr error
	post, prepareErr = content.PreparePost(ctx, scope.caller.Organization, post)
	if prepareErr != nil {
		return threadstore.Post{}, prepareErr
	}
	err := s.deps.Repository.WriteTenant(ctx, scope.caller.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := scope.authorize(ctx, w, true); err != nil {
			return err
		}
		f, err := s.threadRecordFilter(ctx, w, scope)
		if err != nil {
			return err
		}
		if _, err = w.Thread(ctx, thread.ID, f); err != nil {
			return err
		}
		post.References, err = s.validateThreadRecordReferences(ctx, w, scope, in.References)
		if err != nil {
			return err
		}
		post.InputHash, err = hubapi.CommandHash(struct {
			Kind, Body string
			References []threadstore.Reference
		}{in.Kind, in.Body, post.References})
		if err != nil {
			return err
		}
		post, err = w.AppendThreadPost(ctx, thread.ID, post)
		return err
	})
	if err != nil {
		return threadstore.Post{}, s.recordError(err)
	}
	post, err = content.ReadPost(ctx, scope.caller.Organization, post)
	return post.Post, err
}
func (s *Service) getThreadRecord(ctx context.Context, scope access, thread threadstore.Thread, in operatortool.ThreadArguments) (threadstore.Read, error) {
	out := threadstore.Read{Thread: thread, Posts: []threadstore.Post{}}
	principal := scope.caller.Principal
	var after, cursor int64
	var posts []threadstore.PostRecord
	err := s.deps.Repository.ReadTenant(ctx, scope.caller.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.authorize(ctx, r, false); err != nil {
			return err
		}
		f, err := s.threadRecordFilter(ctx, r, scope)
		if err != nil {
			return err
		}
		current, err := r.Thread(ctx, thread.ID, f)
		if err != nil {
			return err
		}
		thread.PostCount = current.PostCount
		out.Thread.PostCount = current.PostCount
		if in.Since == "last_read" {
			after, err = r.ThreadReadCursor(ctx, thread.ID, principal)
		} else if in.Since != "" {
			after, err = strconv.ParseInt(in.Since, 10, 64)
		}
		if err != nil || after < 0 || after > thread.PostCount {
			return Invalid()
		}
		cursor = after
		posts, err = r.ThreadPosts(ctx, thread.ID, after, thread.PostCount, Limit(in.Limit))
		if err != nil {
			return err
		}
		for i := range posts {
			posts[i].References, err = s.threadRecordReferences(ctx, r, scope, thread.ID, posts[i].Sequence)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return out, s.recordError(err)
	}
	if err := s.HydratePosts(ctx, scope.caller.Organization, posts); err != nil {
		return out, err
	}
	bytes := 0
	for _, post := range posts {
		if bytes+len(post.Body) > 24*1024 {
			break
		}
		bytes += len(post.Body)
		out.Posts = append(out.Posts, post.Post)
		encoded, err := json.Marshal(out)
		if err != nil {
			return out, err
		}
		if len(encoded) > operatortool.MaxResultBytes/2 {
			out.Posts = out.Posts[:len(out.Posts)-1]
			break
		}
		cursor = post.Sequence
	}
	err = s.deps.Repository.ReadTenant(ctx, scope.caller.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.authorize(ctx, r, false); err != nil {
			return err
		}
		f, err := s.threadRecordFilter(ctx, r, scope)
		if err != nil {
			return err
		}
		current, err := r.Thread(ctx, thread.ID, f)
		if err != nil {
			return err
		}
		updated := current.Thread
		if err := s.fillThreadRecord(ctx, r, scope, &updated); err != nil {
			return err
		}
		updated.Preview = out.Thread.Preview
		out.Thread = updated
		for i := range out.Posts {
			out.Posts[i].References, err = s.threadRecordReferences(ctx, r, scope, thread.ID, out.Posts[i].Sequence)
			if err != nil {
				return err
			}
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if len(encoded) > operatortool.MaxResultBytes/2 {
			return Invalid()
		}
		return nil
	})
	if err != nil {
		return threadstore.Read{}, s.recordError(err)
	}
	err = s.deps.Repository.WriteTenant(ctx, scope.caller.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := scope.authorize(ctx, w, false); err != nil {
			return err
		}
		f, err := s.threadRecordFilter(ctx, w, scope)
		if err != nil {
			return err
		}
		if _, err := w.Thread(ctx, thread.ID, f); err != nil {
			return err
		}
		return w.AdvanceThreadRead(ctx, thread.ID, principal, cursor)
	})
	if err != nil {
		return threadstore.Read{}, s.recordError(err)
	}
	out.Cursor = strconv.FormatInt(cursor, 10)
	out.HasMore = cursor < out.Thread.PostCount
	out.Thread.Unread = out.Thread.PostCount - cursor
	return out, nil
}
func (s *Service) HydratePosts(ctx context.Context, organization tracker.OrganizationID, posts []threadstore.PostRecord) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(posts))
	for i := range posts {
		jobs <- i
	}
	close(jobs)
	failures := make([]error, len(posts))
	content := threadstore.NewContent(s.deps.Details())
	var workers sync.WaitGroup
	for range min(4, len(posts)) {
		workers.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				post, err := content.ReadPost(ctx, organization, posts[i])
				if err != nil {
					failures[i] = err
					cancel()
					continue
				}
				posts[i] = post
			}
		})
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return err
	}
	return ctx.Err()
}
