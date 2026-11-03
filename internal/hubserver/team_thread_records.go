package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func threadRecordError(err error) error {
	if errors.Is(err, threadstore.ErrIdempotencyConflict) {
		return &nativeError{Code: "idempotency_conflict", Message: "Thread request key was already used", status: http.StatusConflict, publicMessage: true}
	}
	if errors.Is(err, corestore.ErrInvalidPage) {
		return threadInvalid()
	}
	return projectRecordError(err)
}
func (s *Service) threadRecordFilter(ctx context.Context, r tenantstore.Reader, scope nativeScope) (threadstore.Filter, error) {
	f := threadstore.Filter{Principal: operatorIdentity(scope.credential, string(scope.organization)).PrincipalID, AllProjects: scope.credential.Hosted == nil && scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly}
	if scope.credential.HostedRole == "reporter" {
		f.Reporter = scope.actor().PrincipalID
		if f.Reporter == "" {
			return f, operatortool.ErrAccessDenied
		}
	}
	if !f.AllProjects {
		if scope.credential.Hosted == nil {
			grants, err := r.AccessTokenGrants(ctx, scope.credential.ID, "", 200)
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
			rows, err := reader.ProjectAccessPage(ctx, projectAccessFilter(scope.credential, "", "", 200))
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
func (s *Service) threadRecordReference(ctx context.Context, r tenantstore.Reader, scope nativeScope, kind, id string) (teamReference, error) {
	target, err := r.ThreadReferenceTarget(ctx, kind, id)
	if err != nil {
		return teamReference{}, err
	}
	scope.project = tracker.ProjectID(target.ProjectID)
	if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
		return teamReference{}, err
	}
	switch kind {
	case "issue":
		err = requireDashboardRecordItem(ctx, r, scope, id)
	case "change":
		err = requireDashboardRecordItem(ctx, r, scope, target.IssueID)
	case "attachment":
		var metadata attachment.Metadata
		err = s.readAttachmentRecordWith(ctx, r, scope, id, &metadata)
	default:
		return teamReference{}, threadInvalid()
	}
	return target.Reference, err
}
func (s *Service) threadRecordReferences(ctx context.Context, r tenantstore.Reader, scope nativeScope, id string, seq int64) ([]teamReference, error) {
	refs, err := r.ThreadReferences(ctx, id, seq)
	if err != nil {
		return nil, err
	}
	out := []teamReference{}
	for _, ref := range refs {
		current, err := s.threadRecordReference(ctx, r, scope, ref.Kind, ref.ID)
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || isNativeNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, current)
	}
	return out, nil
}
func (s *Service) fillThreadRecord(ctx context.Context, r tenantstore.Reader, scope nativeScope, t *teamThread) error {
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
			t.References = append([]teamReference{ref}, t.References...)
		}
	}
	return nil
}
func (s *Service) threadRecordAuthor(ctx context.Context, scope nativeScope) (string, string, string, string) {
	principal := operatorIdentity(scope.credential, string(scope.organization)).PrincipalID
	name := scope.credential.Name
	if scope.credential.Hosted != nil {
		name = strings.TrimSpace(scope.credential.Hosted.FirstName + " " + scope.credential.Hosted.LastName)
	}
	if name == "" && scope.credential.Hosted != nil {
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			member, err := r.AccessMember(ctx, scope.credential.Hosted.Subject)
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
	if scope.credential.HostedKeyKind == "personal" || scope.credential.EntryKey != nil && scope.credential.EntryKey.Kind == "personal" || scope.credential.Agent != "" {
		acting = principal
		client = scope.credential.Agent
		if client == "" {
			client = operatortool.CurrentConnection(ctx).Client
		}
		if client == "" {
			client = scope.credential.Name
		}
	}
	return principal, name, acting, client
}
func (s *Service) validateThreadRecordReferences(ctx context.Context, r tenantstore.Reader, scope nativeScope, inputs []operatortool.ThreadReference) ([]teamReference, error) {
	refs := []teamReference{}
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
func (s *Service) teamThreadRecordOperation(ctx context.Context, scope nativeScope, name string, in teamThreadInput) (any, error) {
	if name == "list_threads" {
		return s.listThreadRecords(ctx, scope, in)
	}
	if name == "create_thread" {
		return s.createThreadRecord(ctx, scope, in)
	}
	var thread teamThread
	var preview *threadstore.PostRecord
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
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
		return nil, threadRecordError(err)
	}
	if preview != nil {
		post, err := threadstore.NewContent(s.runDetailStore()).ReadPost(ctx, scope.organization, *preview)
		if err != nil {
			return nil, err
		}
		thread.Preview = boundRunes(post.Body, 200)
	}
	scope.project = tracker.ProjectID(thread.ProjectID)
	scope.actionItem = thread.IssueID
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
			return nil, threadInvalid()
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
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
		return thread, threadRecordError(err)
	}
	return nil, operatortool.ErrUnknownTool
}
func (s *Service) listThreadRecords(ctx context.Context, scope nativeScope, in teamThreadInput) (teamThreadPage, error) {
	out := teamThreadPage{Threads: []teamThread{}}
	var previews []threadstore.PostRecord
	var previewIndexes []int
	if in.Scope != "" && in.Scope != "general" && in.Scope != "project" && in.Scope != "issue" || in.Status != "" && in.Status != "open" && in.Status != "resolved" {
		return out, threadInvalid()
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		f, err := s.threadRecordFilter(ctx, r, scope)
		if err != nil {
			return err
		}
		f.Scope, f.ProjectID, f.IssueID, f.Status = in.Scope, tracker.ProjectID(in.ProjectID), in.IssueID, in.Status
		if in.Search != "" {
			terms := strings.FieldsFunc(in.Search, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
			if len(terms) > nativeSearchTermLimit {
				terms = terms[:nativeSearchTermLimit]
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
				return threadInvalid()
			}
			f.BeforeTime, f.BeforeID = cursor[0], cursor[1]
		}
		limit := threadLimit(in.Limit)
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
		return out, threadRecordError(err)
	}
	if err := s.hydrateThreadPosts(ctx, scope.organization, previews); err != nil {
		return out, err
	}
	for i, post := range previews {
		out.Threads[previewIndexes[i]].Preview = boundRunes(post.Body, 200)
	}
	return out, nil
}
func (s *Service) createThreadRecord(ctx context.Context, scope nativeScope, in teamThreadInput) (teamThread, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || utf8.RuneCountInString(in.Title) > 200 {
		return teamThread{}, threadInvalid()
	}
	if in.Key == "" {
		in.Key = newNativeID("intent")
	}
	principal, name, _, client := s.threadRecordAuthor(ctx, scope)
	now := conversationTime(s.config.now())
	record := threadstore.Record{Thread: teamThread{ID: newNativeID("th"), Scope: in.Scope, ProjectID: in.ProjectID, IssueID: in.IssueID, Title: in.Title, Status: "open", Creator: principal, CreatorName: name, CreatorClient: client, CreatedAt: now, UpdatedAt: now, Participants: []teamParticipant{}, References: []teamReference{}}, RequestKey: in.Key}
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		switch in.Scope {
		case "general":
			if in.ProjectID != "" || in.IssueID != "" {
				return threadInvalid()
			}
		case "project":
			if in.ProjectID == "" || in.IssueID != "" {
				return threadInvalid()
			}
		case "issue":
			target, err := w.ThreadReferenceTarget(ctx, "issue", in.IssueID)
			if err != nil {
				return err
			}
			record.ProjectID = target.ProjectID
		case "":
			return threadInvalid()
		default:
			return threadInvalid()
		}
		scope.project = tracker.ProjectID(record.ProjectID)
		scope.actionItem = record.IssueID
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if record.IssueID != "" {
			if err := requireDashboardRecordItem(ctx, w, scope, record.IssueID); err != nil {
				return err
			}
		}
		var err error
		record.References, err = s.validateThreadRecordReferences(ctx, w, scope, in.References)
		if err != nil {
			return err
		}
		record.InputHash, err = nativeCommandHash(struct {
			Scope, ProjectID, IssueID, Title string
			References                       []teamReference
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
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
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
		return teamThread{}, threadRecordError(err)
	}
	if record.PostCount > 0 {
		var posts []threadstore.PostRecord
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
				return err
			}
			var err error
			posts, err = r.ThreadPosts(ctx, record.ID, record.PostCount-1, record.PostCount, 1)
			return err
		})
		if err != nil {
			return teamThread{}, threadRecordError(err)
		}
		if len(posts) != 1 {
			return teamThread{}, nativeNotFound()
		}
		if err := s.hydrateThreadPosts(ctx, scope.organization, posts); err != nil {
			return teamThread{}, err
		}
		record.Preview = boundRunes(posts[0].Body, 200)
	}
	return record.Thread, nil
}
func (s *Service) postThreadRecord(ctx context.Context, scope nativeScope, thread teamThread, in teamThreadInput) (teamPost, error) {
	if strings.TrimSpace(in.Body) == "" || len(in.Body) > 8192 || in.Kind != "note" && in.Kind != "finding" && in.Kind != "question" && in.Kind != "decision" {
		return teamPost{}, threadInvalid()
	}
	if in.Key == "" {
		in.Key = newNativeID("intent")
	}
	principal, name, acting, client := s.threadRecordAuthor(ctx, scope)
	post := threadstore.PostRecord{Post: teamPost{Author: principal, Name: name, ActingFor: acting, Client: client, Kind: in.Kind, Body: in.Body, CreatedAt: conversationTime(s.config.now())}, RequestKey: in.Key}
	content := threadstore.NewContent(s.runDetailStore())
	var prepareErr error
	post, prepareErr = content.PreparePost(ctx, scope.organization, post)
	if prepareErr != nil {
		return teamPost{}, prepareErr
	}
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
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
		post.InputHash, err = nativeCommandHash(struct {
			Kind, Body string
			References []teamReference
		}{in.Kind, in.Body, post.References})
		if err != nil {
			return err
		}
		post, err = w.AppendThreadPost(ctx, thread.ID, post)
		return err
	})
	if err != nil {
		return teamPost{}, threadRecordError(err)
	}
	post, err = content.ReadPost(ctx, scope.organization, post)
	return post.Post, err
}
func (s *Service) getThreadRecord(ctx context.Context, scope nativeScope, thread teamThread, in teamThreadInput) (teamThreadRead, error) {
	out := teamThreadRead{Thread: thread, Posts: []teamPost{}}
	principal := operatorIdentity(scope.credential, string(scope.organization)).PrincipalID
	var after, cursor int64
	var posts []threadstore.PostRecord
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
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
			return threadInvalid()
		}
		cursor = after
		posts, err = r.ThreadPosts(ctx, thread.ID, after, thread.PostCount, threadLimit(in.Limit))
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
		return out, threadRecordError(err)
	}
	if err := s.hydrateThreadPosts(ctx, scope.organization, posts); err != nil {
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
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
			return err
		}
		f, err := s.threadRecordFilter(ctx, w, scope)
		if err != nil {
			return err
		}
		if _, err = w.Thread(ctx, thread.ID, f); err != nil {
			return err
		}
		return w.AdvanceThreadRead(ctx, thread.ID, principal, cursor)
	})
	out.Cursor = strconv.FormatInt(cursor, 10)
	out.HasMore = cursor < thread.PostCount
	out.Thread.Unread = thread.PostCount - cursor
	return out, threadRecordError(err)
}
func (s *Service) hydrateThreadPosts(ctx context.Context, organization tracker.OrganizationID, posts []threadstore.PostRecord) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(posts))
	for i := range posts {
		jobs <- i
	}
	close(jobs)
	failures := make([]error, len(posts))
	content := threadstore.NewContent(s.runDetailStore())
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
