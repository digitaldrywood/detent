package conversations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RegisterAPIRoutes(e *echo.Group, deps HTTPDeps) {
	h := NewHTTP(deps)
	scope := h.deps.Scope()
	e.POST(hubapi.NativeBase+"/conversations", h.createConversation, scope, h.deps.Operator)
	e.GET(hubapi.NativeBase+"/conversations", h.listProjectConversations, scope)
	e.GET("/api/v2/organizations/:organization/conversations", h.listOrganizationConversations, h.deps.Organization)
	e.GET(hubapi.NativeBase+"/conversations/:conversation", h.Get, scope)
	e.GET(hubapi.NativeBase+"/work-items/:item/conversation", h.getWorkItemConversation, scope)
	e.GET(hubapi.NativeBase+"/conversations/:conversation/messages", h.listConversationMessages, scope)
	e.GET(hubapi.NativeBase+"/conversations/:conversation/events", h.StreamEvents, scope)
	e.POST(hubapi.NativeBase+"/conversations/:conversation/actions", h.deps.Action, scope, h.deps.Operator)
	e.POST(hubapi.NativeBase+"/conversations/:conversation/commands", h.postConversationCommand, scope, h.deps.Operator)
	e.POST(hubapi.NativeBase+"/conversations/:conversation/link", h.linkConversation, scope)
	e.PATCH(hubapi.NativeBase+"/conversations/:conversation", h.patchConversation, scope)
	e.POST(hubapi.NativeBase+"/conversations/:conversation/archive", h.Archive, scope)
	e.DELETE(hubapi.NativeBase+"/conversations/:conversation", h.Delete, scope)
	e.GET(hubapi.NativeBase+"/work-items/:item/references", h.listWorkItemReferences, scope)
	e.POST(hubapi.NativeBase+"/conversations/:conversation/attachments", h.uploadConversationAttachment, scope)
	// The download also answers a worker: the runner fetches the files the
	// control named with its own token, so the conversation's read rule
	// governs the bytes the provider sees.
	e.GET(hubapi.NativeBase+"/conversations/:conversation/attachments/:attachment", h.getConversationAttachment, h.deps.Scope(hubapi.APIScopeWorker))
	e.DELETE(hubapi.NativeBase+"/conversations/:conversation/attachments/:attachment", h.deleteConversationAttachment, scope)
}

func conversationQueryInt(c echo.Context, name string) (int64, error) {
	raw := strings.TrimSpace(c.QueryParam(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, hubapi.NativePublicInvalid("Query parameter " + name + " must be a non-negative integer")
	}
	return value, nil
}

// conversationQueryTristate reads an optional boolean filter: absent lists
// both groups, true or false restricts to one.
// present is false when the parameter was not given.
func conversationQueryTristate(c echo.Context, name string) (value, present bool, err error) {
	switch strings.ToLower(strings.TrimSpace(c.QueryParam(name))) {
	case "":
		return false, false, nil
	case "1", "true", "yes":
		return true, true, nil
	case "0", "false", "no":
		return false, true, nil
	default:
		return false, false, hubapi.NativePublicInvalid("Query parameter " + name + " must be true or false")
	}
}

// createConversation implements POST /conversations. The mutation is
// idempotent by the request's top-level key through nativeMutation, which
// also applies the hosted feature and growth checks that every native
// mutation runs.
func (h *HTTP) createConversation(c echo.Context) error {
	var request ConversationCreateRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return h.deps.InvalidRequest(c, err)
	}
	value, err := h.server().CommandCreateConversation(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), request)
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusCreated, value)
}

func (h *HTTP) listConversationsPage(c echo.Context, filter ConversationListQuery) error {
	limit, err := conversationQueryInt(c, "limit")
	if err != nil {
		return h.deps.Error(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	filter.Organization = scope.Organization
	filter.Principal = scope.Credential.ID
	filter.Limit = int(limit)
	filter.Cursor = strings.TrimSpace(c.QueryParam("cursor"))
	settled, present, err := conversationQueryTristate(c, "settled")
	if err != nil {
		return h.deps.Error(c, err)
	}
	if present {
		filter.Settled = &settled
	}
	filter.Title = strings.TrimSpace(c.QueryParam("q"))
	filter.SubjectWorkItemID = strings.TrimSpace(c.QueryParam("subject_work_item_id"))
	value, err := h.server().ReadConversationsPage(c.Request().Context(), ScopeInput(scope), filter)
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// listProjectConversations implements GET /conversations for a project.
func (h *HTTP) listProjectConversations(c echo.Context) error {
	return h.listConversationsPage(c, ConversationListQuery{Project: hubscope.NativeRequestScope(c).Project})
}

// listOrganizationConversations implements the organization-level listing:
// the union of the projects the actor can read, most recent activity first.
func (h *HTTP) listOrganizationConversations(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Hosted == nil && scope.Credential.EntryKey == nil {
		return h.listConversationsPage(c, ConversationListQuery{})
	}

	projects, every, err := h.server().ReadableConversationProjects(c.Request().Context(), ScopeInput(scope))
	if err != nil {
		return h.deps.Error(c, err)
	}
	filter := ConversationListQuery{}
	if !every {
		// A non-nil slice, empty when nothing is readable, so the query
		// restricts rather than listing the whole organization.
		filter.Projects = projects
	}
	return h.listConversationsPage(c, filter)
}

func (h *HTTP) getWorkItemConversation(c echo.Context) error {
	if c.QueryParam("metadata") == "true" {

		scope := hubscope.NativeRequestScope(c)
		var record ConversationRecord
		err := h.server().config().TenantRepository.ReadTenant(c.Request().Context(), scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			stored, err := reader.ConversationByWorkItem(c.Request().Context(), scope.Project, c.Param("item"))
			if err != nil {
				return err
			}
			record = ConversationRecordFromStore(stored)
			return nil
		})
		if err != nil {
			return h.deps.Error(c, TranslateConversationError(err))
		}
		record, err = h.server().ReadPGConversation(c.Request().Context(), ScopeInput(scope), record.ID)
		if err != nil {
			return h.deps.Error(c, err)
		}
		return c.JSON(http.StatusOK, ConversationSnapshot{Skills: []skills.ProviderSkill{}, Conversation: ProjectConversation(record), Messages: []ConversationMessageResource{}, Questions: []conversation.Question{}, Cursor: record.EventSeq})
	}
	value, err := h.server().ReadWorkItemConversationSnapshot(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("item"))
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// Get implements GET /conversations/:conversation. The record,
// the latest message page and the pending questions are read in one
// transaction so that the returned cursor matches the messages exactly.
func (h *HTTP) Get(c echo.Context) error {
	value, err := h.server().ReadConversationSnapshot(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("conversation"))
	if err != nil {
		return h.deps.Error(c, err)
	}
	return h.archiveJSON(c, http.StatusOK, value)
}

// listConversationMessages implements GET /conversations/:conversation/messages.
func (h *HTTP) listConversationMessages(c echo.Context) error {
	before, err := conversationQueryInt(c, "before")
	if err != nil {
		return h.deps.Error(c, err)
	}
	limit, err := conversationQueryInt(c, "limit")
	if err != nil {
		return h.deps.Error(c, err)
	}
	if limit == 0 {
		limit = ConversationMessagePage
	}
	value, err := h.server().ReadConversationMessages(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("conversation"), before, int(limit))
	if err != nil {
		return h.deps.Error(c, err)
	}
	return h.archiveJSON(c, http.StatusOK, value)
}

// postConversationCommand implements POST /conversations/:conversation/commands.
func (h *HTTP) postConversationCommand(c echo.Context) error {
	var command conversation.Command
	if err := hubapi.DecodeAPIJSON(c, &command); err != nil {
		return h.deps.InvalidRequest(c, err)
	}
	value, err := h.server().CommandPostConversation(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("conversation"), command)
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

func (h *HTTP) linkConversation(c echo.Context) error {
	var request ConversationLinkRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return h.deps.InvalidRequest(c, err)
	}
	value, err := h.server().CommandLinkConversation(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("conversation"), request)
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// patchConversation implements PATCH /conversations/:conversation. It
// renames a conversation, changes its turn preferences, or both; a request
// that asks for neither is refused rather than silently accepted.
func (h *HTTP) patchConversation(c echo.Context) error {
	var request ConversationPatchRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return h.deps.InvalidRequest(c, err)
	}
	value, err := h.server().CommandPatchConversation(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("conversation"), request)
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// StreamEvents implements GET /conversations/:conversation/events.
// The durable event log is the source: subscribers are only woken, then read
// from their cursor, so a slow client never loses or reorders events. The
// stream re-authorizes on every wake and on a timer and closes on loss.
func (h *HTTP) StreamEvents(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	after, err := conversationQueryInt(c, "after")
	if err != nil {
		return h.deps.Error(c, err)
	}
	service := h.deps.Service()
	ctx := c.Request().Context()
	var record ConversationRecord

	record, err = h.server().ReadPGConversation(ctx, ScopeInput(scope), c.Param("conversation"))

	if err != nil {
		return h.deps.Error(c, err)
	}
	ctx = hubapi.ConnectionContext(ctx)
	stream := NewConversationStream(c)
	stream.Now = h.server().config().Now
	if after > record.EventSeq {
		return stream.Closed(ConversationClosedCursorExpired)
	}
	subscription, cancel := service.Broker.Subscribe(record.ID)
	defer cancel()
	heartbeat := time.NewTicker(h.deps.StreamHeartbeat())
	defer heartbeat.Stop()
	authorize := time.NewTicker(h.deps.StreamAuthorize())
	defer authorize.Stop()
	cursor := after
	for {
		poll, cancelPoll := ctx, func() {}
		if _, bounded := ctx.Deadline(); !bounded {
			poll, cancelPoll = context.WithTimeout(ctx, h.deps.RequestBudget)
		}
		if scope.Credential.EntryKey != nil {
			stream.Authorize = func() error {
				_, err := h.ReauthorizeStream(poll, c, &scope, record.ID)
				return err
			}
		}
		// The re-read proves the stream may continue: a settled conversation
		// keeps streaming, only a revoked audience closes it.
		stream.NextAuthorization = stream.Now().Add(h.deps.StreamAuthorize())
		authorized, err := h.ReauthorizeStream(poll, c, &scope, record.ID)
		if err != nil {
			cancelPoll()
			reason := StreamCloseReason(err)
			service.Logger.Warn("conversation.stream_reauthorize_failed", "conversation_id", record.ID, "reason", reason, "error", err)
			return stream.Closed(reason)
		}

		cursor, err = h.server().ReplayPGConversationEvents(poll, ScopeInput(scope), stream, record.ID, cursor, authorized.EventSeq)
		cancelPoll()

		if err != nil {
			if errors.Is(err, ErrConversationCursorExpired) {
				return stream.Closed(ConversationClosedCursorExpired)
			}
			return stream.Closed(StreamCloseReason(err))
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-subscription.Closed():
				return stream.Closed(ConversationClosedShutdown)
			case <-heartbeat.C:
				if err := stream.Heartbeat(cursor); err != nil {
					return nil
				}
			case <-subscription.Wake():
				break wait
			case <-authorize.C:
				break wait
			}
		}
	}
}

// ReauthorizeStream re-runs the request authorization from live
// state: the hosted session, membership and grant for sessions, token
// revocation and grants for API tokens, then the conversation read rule.
// It updates scope with the refreshed credential and returns the current
// record.
func (h *HTTP) ReauthorizeStream(ctx context.Context, c echo.Context, scope *hubscope.NativeScope, conversationID string) (ConversationRecord, error) {
	if err := h.deps.RefreshStream(ctx, c, scope); err != nil {
		return ConversationRecord{}, err
	}
	return h.server().ReadPGConversation(ctx, ScopeInput(*scope), conversationID)
}

// StreamCloseReason tells a denial apart from a failure. A
// denial is a decision the client must respect; a failure is transient and
// the client may reconnect.
func StreamCloseReason(err error) string {
	err = hubapi.FromFeatureError(err)
	var native *hubapi.NativeError
	switch {
	case errors.Is(err, tenantauth.ErrUnavailable):
		return ConversationClosedServerError
	case errors.As(err, &native):
		return ConversationClosedAccessRevoked
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, auth.ErrHostedIdentity), errors.Is(err, accessstore.ErrDenied), errors.Is(err, operatortool.ErrAccessDenied):
		return ConversationClosedAccessRevoked
	default:
		return ConversationClosedServerError
	}
}

// readConversationAttachmentUpload streams the multipart body. The parts are
// read rather than parsed into temporary files, so a refused upload never
// reaches the disk. Only the two documented fields are accepted.
func readConversationAttachmentUpload(c echo.Context) (ConversationAttachmentUpload, error) {
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, ConversationAttachmentRequestBytes)
	reader, err := request.MultipartReader()
	if err != nil {
		return ConversationAttachmentUpload{}, hubapi.NativePublicInvalid("A multipart upload with a file part is required")
	}
	var upload ConversationAttachmentUpload
	seenFile := false
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ConversationAttachmentUpload{}, AttachmentBodyError(err)
		}
		switch part.FormName() {
		case "file":
			if seenFile {
				return ConversationAttachmentUpload{}, conversationAttachmentRefusal(part, "An upload carries one file")
			}
			seenFile = true
			upload.Name = part.FileName()
			upload.Media = part.Header.Get(echo.HeaderContentType)
			// One byte over the limit is enough to know the file is too big.
			upload.Content, err = readConversationAttachmentPart(part, conversation.MaxAttachmentBytes+1)
			if err != nil {
				return ConversationAttachmentUpload{}, AttachmentBodyError(err)
			}
			if int64(len(upload.Content)) > conversation.MaxAttachmentBytes {
				return ConversationAttachmentUpload{}, conversationAttachmentTooLarge()
			}
		case "idempotency_key":
			value, err := readConversationAttachmentPart(part, ConversationAttachmentFieldBytes)
			if err != nil {
				return ConversationAttachmentUpload{}, AttachmentBodyError(err)
			}
			upload.Key = strings.TrimSpace(string(value))
		default:
			return ConversationAttachmentUpload{}, conversationAttachmentRefusal(part, "Field "+part.FormName()+" is not part of an attachment upload")
		}
	}
	if !seenFile {
		return ConversationAttachmentUpload{}, hubapi.NativePublicInvalid("A file part is required")
	}
	if upload.Key == "" || len(upload.Key) > conversation.MaxCommandKeyBytes {
		return ConversationAttachmentUpload{}, hubapi.NativePublicInvalid(fmt.Sprintf("An idempotency key of at most %d bytes is required", conversation.MaxCommandKeyBytes))
	}
	return upload, nil
}

// readConversationAttachmentPart reads at most limit bytes of a part and
// closes it. The close belongs to the read: a part that will not close has
// not been read cleanly, so its failure joins the read's.
func readConversationAttachmentPart(part *multipart.Part, limit int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(part, limit))
	return content, errors.Join(err, part.Close())
}

// conversationAttachmentRefusal closes the part a refused upload leaves
// unread and returns the refusal. Closing only discards the rest of that
// part, so a close that fails means the body itself is broken and says so
// instead.
func conversationAttachmentRefusal(part *multipart.Part, message string) error {
	if err := part.Close(); err != nil {
		return AttachmentBodyError(err)
	}
	return hubapi.NativeInvalid(message)
}

func conversationAttachmentTooLarge() error {
	return &hubapi.NativeError{Code: "invalid_request", Message: fmt.Sprintf("A file must be at most %d bytes", conversation.MaxAttachmentBytes), Status: http.StatusRequestEntityTooLarge, PublicMessage: true}
}

// AttachmentBodyError turns a truncated or oversized body into the
// API error it deserves; anything else is the transport failing.
func AttachmentBodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return conversationAttachmentTooLarge()
	}
	return hubapi.NativePublicInvalid("The upload could not be read")
}

// uploadConversationAttachment implements POST
// {nativeBase}/conversations/:conversation/attachments.
func (h *HTTP) uploadConversationAttachment(c echo.Context) error {
	upload, err := readConversationAttachmentUpload(c)
	if err != nil {
		return h.deps.Error(c, err)
	}
	value, err := h.server().CommandUploadAttachment(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("conversation"), upload)
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusCreated, value)
}

// getConversationAttachment implements GET
// {nativeBase}/conversations/:conversation/attachments/:attachment. The
// audience is the conversation's read rule.
func (h *HTTP) getConversationAttachment(c echo.Context) error {
	attachment, content, err := h.server().ReadAttachmentPG(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("conversation"), c.Param("attachment"))
	if err != nil {
		return h.deps.Error(c, err)
	}
	// The name is a header parameter, never part of the path, so a crafted
	// file name cannot forge a header.
	c.Response().Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Name}))
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Blob(http.StatusOK, attachment.MIME, content)
}

// deleteConversationAttachment implements DELETE
// {nativeBase}/conversations/:conversation/attachments/:attachment. Only the
// owner may delete, and only while the attachment is unsent.
func (h *HTTP) deleteConversationAttachment(c echo.Context) error {
	_, err := h.server().CommandDeleteAttachment(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("conversation"), c.Param("attachment"))
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *HTTP) Archive(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	id := c.Param("conversation")
	original, err := h.deps.Service().ReadPublicationConversationForOrganization(ctx, scope.Organization, id)
	if err != nil {
		return h.deps.Error(c, err)
	}
	if err := h.server().config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return h.server().AuthorizeConversationRemoval(ctx, r, ScopeInput(scope), original)
	}); err != nil {
		return h.deps.Error(c, err)
	}
	if err := RequireConversationRemoval(original); err != nil {
		return h.deps.Error(c, err)
	}
	record := original
	record.Archived, record.Revision, record.EventSeq, record.UpdatedAt = true, original.Revision+1, original.EventSeq+1, h.server().Now()
	raw, err := json.Marshal(ProjectConversation(record))
	if err != nil {
		return h.deps.Error(c, err)
	}
	payload, err := MessageContent(ConversationMessageRecord{Data: raw})
	if err != nil {
		return h.deps.Error(c, err)
	}
	ref, err := h.deps.Service().Store.PublishPayload(ctx, string(scope.Organization), payload)
	if err != nil {
		return h.deps.Error(c, err)
	}
	contentID := ContentArchiveID(string(scope.Organization), ref)
	err = h.server().config().TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.LockConversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if err := h.server().AuthorizeConversationRemoval(ctx, w, ScopeInput(scope), ConversationRecordFromStore(stored)); err != nil {
			return err
		}
		if stored.Revision != original.Revision || stored.EventSequence != original.EventSeq {
			return hubapi.NativeConflict(tracker.Revision(stored.Revision))
		}
		if err := RequireConversationRemoval(ConversationRecordFromStore(stored)); err != nil {
			return err
		}
		stored.Execution, stored.Preferences = original.Execution, original.Preferences
		stored.Archived, stored.UpdatedAt = true, record.UpdatedAt
		if _, err := w.UpdateConversation(ctx, stored, stored.Revision); err != nil {
			return err
		}
		if err := w.PutConversationContent(ctx, conversationstore.ContentObject{OrganizationID: scope.Organization, ID: contentID, Reference: ref, CreatedAt: record.UpdatedAt}); err != nil {
			return err
		}
		event, err := w.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: scope.Organization, ConversationID: id, ContentReference: contentID, Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{}`), CreatedAt: record.UpdatedAt}})
		record.EventSeq = event.Seq
		return err
	})
	if err != nil {
		return h.deps.Error(c, TranslateConversationError(err))
	}
	h.deps.Service().Committed(record)
	return c.JSON(http.StatusOK, ProjectConversation(record))
}

func (h *HTTP) Delete(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	ctx := c.Request().Context()
	id := c.Param("conversation")
	original, err := h.deps.Service().ReadPublicationConversationForOrganization(ctx, scope.Organization, id)
	if err != nil {
		return h.deps.Error(c, err)
	}
	err = h.server().config().TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.LockConversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		record := ConversationRecordFromStore(stored)
		if err := h.server().AuthorizeConversationRemoval(ctx, w, ScopeInput(scope), record); err != nil {
			return err
		}
		if stored.Revision != original.Revision || stored.EventSequence != original.EventSeq {
			return hubapi.NativeConflict(tracker.Revision(stored.Revision))
		}
		if err := RequireConversationRemoval(record); err != nil {
			return err
		}
		if err := RequireConversationRemoval(original); err != nil {
			return err
		}
		owner, ok := w.(conversationstore.RemovalWriter)
		if !ok {
			return errors.New("conversation removal owner is required")
		}
		return owner.DeleteConversation(ctx, scope.Project, id, h.server().Now())
	})
	if err != nil {
		return h.deps.Error(c, TranslateConversationError(err))
	}
	h.deps.Service().Broker.Notify(id)
	return c.NoContent(http.StatusNoContent)
}

// listWorkItemReferences implements GET /work-items/:item/references. The
// scope middleware has already proved the reader can read the project; the
// issue read proves the issue exists in it, and the conversation read rule
// drops entries from private chats the reader does not own.
func (h *HTTP) listWorkItemReferences(c echo.Context) error {
	value, err := h.server().ReadWorkItemReferences(c.Request().Context(), ScopeInput(hubscope.NativeRequestScope(c)), c.Param("item"))
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

type HTTPDeps struct {
	Service         func() *ConversationService
	Scope           func(...hubapi.APIScope) echo.MiddlewareFunc
	Organization    echo.MiddlewareFunc
	Operator        echo.MiddlewareFunc
	Worker          echo.MiddlewareFunc
	Action          echo.HandlerFunc
	Error           func(echo.Context, error) error
	InvalidRequest  func(echo.Context, error) error
	RefreshStream   func(context.Context, echo.Context, *hubscope.NativeScope) error
	StreamHeartbeat func() time.Duration
	StreamAuthorize func() time.Duration
	RequestBudget   time.Duration
}

type HTTP struct{ deps HTTPDeps }

func NewHTTP(deps HTTPDeps) *HTTP { return &HTTP{deps: deps} }

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	RegisterAPIRoutes(e, deps)
	deps.Service().RegisterWorkerRoutes(e, deps.Worker)
}

func (h *HTTP) server() *Server { return h.deps.Service().Server }

func (h *HTTP) archiveJSON(c echo.Context, status int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw, err = h.server().deps.WithArchive(c.Request().Context(), raw)
	if err != nil {
		return err
	}
	return c.JSONBlob(status, raw)
}
