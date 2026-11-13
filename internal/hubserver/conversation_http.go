package hubserver

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
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// registerConversationAPIRoutes mounts the operator-facing conversation API
// (the conversation contract). Hosted sessions authenticate as operator credentials.
//
// The project routes use requireConversationScope rather than
// requireNativeScope: the native middleware turns a hosted read-only grant
// into an opaque 404 on every mutation, while the conversation contract
// distinguishes "cannot see" (404) from "can read but cannot control" (403).
// Write capability is therefore checked per handler by authorizeWrite.
var (
	conversationStreamHeartbeat = conversationHeartbeatInterval
	conversationStreamAuthorize = conversationAuthorizeInterval
)

func (s *Service) registerConversationAPIRoutes(e *echo.Echo) {
	scope := s.requireConversationScope()
	e.POST(nativeBase+"/conversations", s.createConversation, scope, s.operatorAuthority)
	e.GET(nativeBase+"/conversations", s.listProjectConversations, scope)
	e.GET("/api/v2/organizations/:organization/conversations", s.listOrganizationConversations, s.requireConversationOrganization())
	e.GET(nativeBase+"/conversations/:conversation", s.getConversation, scope)
	e.GET(nativeBase+"/work-items/:item/conversation", s.getWorkItemConversation, scope)
	e.GET(nativeBase+"/conversations/:conversation/messages", s.listConversationMessages, scope)
	e.GET(nativeBase+"/conversations/:conversation/events", s.streamConversationEvents, scope)
	e.POST(nativeBase+"/conversations/:conversation/actions", s.postConversationAction, scope, s.operatorAuthority)
	e.POST(nativeBase+"/conversations/:conversation/commands", s.postConversationCommand, scope, s.operatorAuthority)
	e.POST(nativeBase+"/conversations/:conversation/link", s.linkConversation, scope)
	e.PATCH(nativeBase+"/conversations/:conversation", s.patchConversation, scope)
	e.POST(nativeBase+"/conversations/:conversation/archive", s.archiveConversation, scope)
	e.DELETE(nativeBase+"/conversations/:conversation", s.deleteConversation, scope)
	e.GET(nativeBase+"/work-items/:item/references", s.listWorkItemReferences, scope)
	e.POST(nativeBase+"/conversations/:conversation/attachments", s.uploadConversationAttachment, scope)
	// The download also answers a worker: the runner fetches the files the
	// control named with its own token, so the conversation's read rule
	// governs the bytes the provider sees.
	e.GET(nativeBase+"/conversations/:conversation/attachments/:attachment", s.getConversationAttachment, s.requireConversationScope(apiScopeWorker))
	e.DELETE(nativeBase+"/conversations/:conversation/attachments/:attachment", s.deleteConversationAttachment, scope)
}

// requireConversationScope authenticates an operator or admin credential
// for a project route and requires read access to the project. It mirrors
// requireNativeScope except that hosted write capability is left to the
// handlers so that a read-only member receives 403 instead of 404. Extra
// scopes widen the credential set; the per-handler authorization is
// unchanged, so a wider route is still bounded by the conversation's own
// read rule.
func (s *Service) requireConversationScope(extra ...apiScope) echo.MiddlewareFunc {
	allowed := append([]apiScope{apiScopeOperator, apiScopeAdmin}, extra...)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireAPIScope(allowed...)(func(c echo.Context) error {
			credential, ok := c.Get("hub_api_credential").(apiCredential)
			if !ok {
				return s.nativeAPIError(c, nativeNotFound())
			}
			ctx := c.Request().Context()
			scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project")), credential: credential}

			if err := s.authorizeProjectRecord(ctx, scope, false); err != nil {
				if c.Param("conversation") != "" {
					var denied *nativeError
					if errors.Is(err, accessstore.ErrDenied) || errors.As(err, &denied) && denied.status == http.StatusForbidden {
						return s.nativeAPIError(c, nativeNotFound())
					}
				}
				return s.nativeAPIError(c, err)
			}
			if _, err := s.readProjectRecord(ctx, scope); err != nil {
				return s.nativeAPIError(c, err)
			}
			if item := c.Param("item"); item != "" {
				if _, _, err := s.readWorkItemRecord(ctx, scope, item); err != nil {
					return s.nativeAPIError(c, err)
				}
			}

			c.Set("native_scope", scope)
			if c.Request().Method == http.MethodGet {
				c.SetRequest(c.Request().WithContext(s.archiveRequestContext(c, scope)))
			}
			c.Response().Header().Set("Cache-Control", "no-store")
			if credential.Hosted != nil {
				if err := s.hostedAudit(ctx, credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.project), 0); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			return next(c)
		})
	}
}

// requireConversationOrganization authenticates an operator or admin
// credential for the organization-level listing, which has no :project.
// requireAPIScope cannot be reused: in hosted mode it rejects session
// credentials outside the native project base.
func (s *Service) requireConversationOrganization() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, status, err := s.authenticateAPIRequest(c)
			if err != nil {
				return s.refusedAPIError(c, status, "unauthorized", "Valid scoped API token is required", apiErrorResponse{Code: "unauthorized", Message: "Valid scoped API token is required"})
			}
			if s.config.Hosted != nil && credential.Hosted == nil {
				return s.nativeAPIError(c, nativeNotFound())
			}
			if credential.Scope != apiScopeOperator && credential.Scope != apiScopeAdmin {
				return s.refusedAPIError(c, http.StatusForbidden, "insufficient_scope", "API token scope does not permit this operation", apiErrorResponse{Code: "insufficient_scope", Message: "API token scope does not permit this operation"})
			}
			ctx := c.Request().Context()
			scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}
			if err := s.authorizeConversationOrganization(ctx, scope); err != nil {
				return s.nativeAPIError(c, err)
			}
			c.Set("hub_api_credential", credential)
			c.Set("native_scope", scope)
			c.Response().Header().Set("Cache-Control", "no-store")
			if credential.Hosted != nil {
				if err := s.hostedAudit(ctx, credential.Hosted, "action", c.Request().Method+" "+c.Path(), "", 0); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			return next(c)
		}
	}
}

func (s *Service) authorizeConversationOrganization(ctx context.Context, scope nativeScope) error {
	return fromFeatureError(s.requestAuth().AuthorizeOrganization(ctx, s.authInput(scope)))
}

func conversationQueryInt(c echo.Context, name string) (int64, error) {
	raw := strings.TrimSpace(c.QueryParam(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, nativePublicInvalid("Query parameter " + name + " must be a non-negative integer")
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
		return false, false, nativePublicInvalid("Query parameter " + name + " must be true or false")
	}
}

// createConversation implements POST /conversations. The mutation is
// idempotent by the request's top-level key through nativeMutation, which
// also applies the hosted feature and growth checks that every native
// mutation runs.
func (s *Service) createConversation(c echo.Context) error {
	var request conversationCreateRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandCreateConversation(c.Request().Context(), nativeRequestScope(c), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusCreated, value)
}

func (s *Service) listConversationsPage(c echo.Context, filter conversationListQuery) error {
	limit, err := conversationQueryInt(c, "limit")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	filter.Organization = scope.organization
	filter.Principal = scope.credential.ID
	filter.Limit = int(limit)
	filter.Cursor = strings.TrimSpace(c.QueryParam("cursor"))
	settled, present, err := conversationQueryTristate(c, "settled")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if present {
		filter.Settled = &settled
	}
	filter.Title = strings.TrimSpace(c.QueryParam("q"))
	filter.SubjectWorkItemID = strings.TrimSpace(c.QueryParam("subject_work_item_id"))
	value, err := s.readConversationsPage(c.Request().Context(), scope, filter)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// listProjectConversations implements GET /conversations for a project.
func (s *Service) listProjectConversations(c echo.Context) error {
	return s.listConversationsPage(c, conversationListQuery{Project: nativeRequestScope(c).project})
}

// listOrganizationConversations implements the organization-level listing:
// the union of the projects the actor can read, most recent activity first.
func (s *Service) listOrganizationConversations(c echo.Context) error {
	scope := nativeRequestScope(c)
	if scope.credential.Hosted == nil && scope.credential.EntryKey == nil {
		return s.listConversationsPage(c, conversationListQuery{})
	}

	projects, every, err := s.readableConversationProjects(c.Request().Context(), scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	filter := conversationListQuery{}
	if !every {
		// A non-nil slice, empty when nothing is readable, so the query
		// restricts rather than listing the whole organization.
		filter.Projects = projects
	}
	return s.listConversationsPage(c, filter)
}

func (s *Service) getWorkItemConversation(c echo.Context) error {
	if c.QueryParam("metadata") == "true" {

		scope := nativeRequestScope(c)
		var record conversationRecord
		err := s.config.TenantRepository.ReadTenant(c.Request().Context(), scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			stored, err := reader.ConversationByWorkItem(c.Request().Context(), scope.project, c.Param("item"))
			if err != nil {
				return err
			}
			record = conversationRecordFromStore(stored)
			return nil
		})
		if err != nil {
			return s.nativeAPIError(c, translateConversationError(err))
		}
		record, err = s.readPGConversation(c.Request().Context(), scope, record.ID)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, conversationSnapshot{Skills: []skills.ProviderSkill{}, Conversation: projectConversation(record), Messages: []conversationMessageResource{}, Questions: []conversation.Question{}, Cursor: record.EventSeq})
	}
	value, err := s.readWorkItemConversationSnapshot(c.Request().Context(), nativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// getConversation implements GET /conversations/:conversation. The record,
// the latest message page and the pending questions are read in one
// transaction so that the returned cursor matches the messages exactly.
func (s *Service) getConversation(c echo.Context) error {
	value, err := s.readConversationSnapshot(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, value)
}

// listConversationMessages implements GET /conversations/:conversation/messages.
func (s *Service) listConversationMessages(c echo.Context) error {
	before, err := conversationQueryInt(c, "before")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, err := conversationQueryInt(c, "limit")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if limit == 0 {
		limit = conversationMessagePage
	}
	value, err := s.readConversationMessages(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), before, int(limit))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, value)
}

// postConversationCommand implements POST /conversations/:conversation/commands.
func (s *Service) postConversationCommand(c echo.Context) error {
	var command conversation.Command
	if err := decodeAPIJSON(c, &command); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandPostConversation(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), command)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

func (s *Service) linkConversation(c echo.Context) error {
	var request conversationLinkRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandLinkConversation(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// patchConversation implements PATCH /conversations/:conversation. It
// renames a conversation, changes its turn preferences, or both; a request
// that asks for neither is refused rather than silently accepted.
func (s *Service) patchConversation(c echo.Context) error {
	var request conversationPatchRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandPatchConversation(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// streamConversationEvents implements GET /conversations/:conversation/events.
// The durable event log is the source: subscribers are only woken, then read
// from their cursor, so a slow client never loses or reorders events. The
// stream re-authorizes on every wake and on a timer and closes on loss.
func (s *Service) streamConversationEvents(c echo.Context) error {
	scope := nativeRequestScope(c)
	after, err := conversationQueryInt(c, "after")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service := s.conversations
	ctx := c.Request().Context()
	var record conversationRecord

	record, err = s.readPGConversation(ctx, scope, c.Param("conversation"))

	if err != nil {
		return s.nativeAPIError(c, err)
	}
	stream := newConversationStream(c)
	stream.Now = s.config.now
	if scope.credential.EntryKey != nil {
		stream.Authorize = func() error {
			_, err := s.reauthorizeConversationStream(c, &scope, record.ID)
			return err
		}
	}
	if after > record.EventSeq {
		return stream.Closed(conversationClosedCursorExpired)
	}
	subscription, cancel := conversationSubscribe(service.Broker, record.ID)
	defer cancel()
	heartbeat := time.NewTicker(conversationStreamHeartbeat)
	defer heartbeat.Stop()
	authorize := time.NewTicker(conversationStreamAuthorize)
	defer authorize.Stop()
	cursor := after
	for {
		// The re-read proves the stream may continue: a settled conversation
		// keeps streaming, only a revoked audience closes it.
		stream.NextAuthorization = stream.Now().Add(conversationStreamAuthorize)
		authorized, err := s.reauthorizeConversationStream(c, &scope, record.ID)
		if err != nil {
			reason := conversationStreamCloseReason(err)
			service.Logger.Warn("conversation.stream_reauthorize_failed", "conversation_id", record.ID, "reason", reason, "error", err)
			return stream.Closed(reason)
		}

		cursor, err = s.replayPGConversationEvents(ctx, scope, stream, record.ID, cursor, authorized.EventSeq)

		if err != nil {
			if errors.Is(err, errConversationCursorExpired) {
				return stream.Closed(conversationClosedCursorExpired)
			}
			return stream.Closed(conversationStreamCloseReason(err))
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-subscription.closed:
				return stream.Closed(conversationClosedShutdown)
			case <-heartbeat.C:
				if err := stream.Heartbeat(cursor); err != nil {
					return nil
				}
			case <-subscription.wake:
				break wait
			case <-authorize.C:
				break wait
			}
		}
	}
}

// reauthorizeConversationStream re-runs the request authorization from live
// state: the hosted session, membership and grant for sessions, token
// revocation and grants for API tokens, then the conversation read rule.
// It updates scope with the refreshed credential and returns the current
// record.
func (s *Service) reauthorizeConversationStream(c echo.Context, scope *nativeScope, conversationID string) (conversationRecord, error) {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	if scope.credential.EntryKey != nil {
		claims, ok := hostedSharedClaims(c)
		if !ok || claims.Key == nil {
			return conversationRecord{}, auth.ErrHostedIdentity
		}
		encoded, err := json.Marshal(entryauthority.Origin{KeyID: claims.Key.ID, CredentialBinding: claims.Key.CredentialBinding, Subject: claims.Subject, Issuer: claims.Issuer, Generation: claims.Generation})
		if err != nil {
			return conversationRecord{}, err
		}
		credential, err := s.currentEntryOriginCredential(readCtx, string(encoded))
		if err != nil {
			return conversationRecord{}, err
		}
		scope.credential = credential
	} else if scope.credential.Hosted != nil {
		credential, _, err := s.authenticateAPIRequestContext(readCtx, c)
		if err != nil {
			return conversationRecord{}, err
		}
		scope.credential = credential
	}

	if err := s.authorizeProjectRecord(readCtx, *scope, false); err != nil {
		return conversationRecord{}, err
	}
	if _, err := s.readProjectRecord(readCtx, *scope); err != nil {
		return conversationRecord{}, err
	}

	return s.readPGConversation(readCtx, *scope, conversationID)
}

// conversationStreamCloseReason tells a denial apart from a failure. A
// denial is a decision the client must respect; a failure is transient and
// the client may reconnect.
func conversationStreamCloseReason(err error) string {
	err = fromFeatureError(err)
	var native *nativeError
	switch {
	case errors.Is(err, tenantauth.ErrUnavailable):
		return conversationClosedServerError
	case errors.As(err, &native):
		return conversationClosedAccessRevoked
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, auth.ErrHostedIdentity), errors.Is(err, accessstore.ErrDenied), errors.Is(err, operatortool.ErrAccessDenied):
		return conversationClosedAccessRevoked
	default:
		return conversationClosedServerError
	}
}

// readConversationAttachmentUpload streams the multipart body. The parts are
// read rather than parsed into temporary files, so a refused upload never
// reaches the disk. Only the two documented fields are accepted.
func readConversationAttachmentUpload(c echo.Context) (conversationAttachmentUpload, error) {
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, conversationAttachmentRequestBytes)
	reader, err := request.MultipartReader()
	if err != nil {
		return conversationAttachmentUpload{}, nativePublicInvalid("A multipart upload with a file part is required")
	}
	var upload conversationAttachmentUpload
	seenFile := false
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return conversationAttachmentUpload{}, conversationAttachmentBodyError(err)
		}
		switch part.FormName() {
		case "file":
			if seenFile {
				return conversationAttachmentUpload{}, conversationAttachmentRefusal(part, "An upload carries one file")
			}
			seenFile = true
			upload.Name = part.FileName()
			upload.Media = part.Header.Get(echo.HeaderContentType)
			// One byte over the limit is enough to know the file is too big.
			upload.Content, err = readConversationAttachmentPart(part, conversation.MaxAttachmentBytes+1)
			if err != nil {
				return conversationAttachmentUpload{}, conversationAttachmentBodyError(err)
			}
			if int64(len(upload.Content)) > conversation.MaxAttachmentBytes {
				return conversationAttachmentUpload{}, conversationAttachmentTooLarge()
			}
		case "idempotency_key":
			value, err := readConversationAttachmentPart(part, conversationAttachmentFieldBytes)
			if err != nil {
				return conversationAttachmentUpload{}, conversationAttachmentBodyError(err)
			}
			upload.Key = strings.TrimSpace(string(value))
		default:
			return conversationAttachmentUpload{}, conversationAttachmentRefusal(part, "Field "+part.FormName()+" is not part of an attachment upload")
		}
	}
	if !seenFile {
		return conversationAttachmentUpload{}, nativePublicInvalid("A file part is required")
	}
	if upload.Key == "" || len(upload.Key) > conversation.MaxCommandKeyBytes {
		return conversationAttachmentUpload{}, nativePublicInvalid(fmt.Sprintf("An idempotency key of at most %d bytes is required", conversation.MaxCommandKeyBytes))
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
		return conversationAttachmentBodyError(err)
	}
	return nativeInvalid(message)
}

func conversationAttachmentTooLarge() error {
	return &nativeError{Code: "invalid_request", Message: fmt.Sprintf("A file must be at most %d bytes", conversation.MaxAttachmentBytes), status: http.StatusRequestEntityTooLarge, publicMessage: true}
}

// conversationAttachmentBodyError turns a truncated or oversized body into the
// API error it deserves; anything else is the transport failing.
func conversationAttachmentBodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return conversationAttachmentTooLarge()
	}
	return nativePublicInvalid("The upload could not be read")
}

// uploadConversationAttachment implements POST
// {nativeBase}/conversations/:conversation/attachments.
func (s *Service) uploadConversationAttachment(c echo.Context) error {
	upload, err := readConversationAttachmentUpload(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.commandUploadAttachment(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), upload)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusCreated, value)
}

// getConversationAttachment implements GET
// {nativeBase}/conversations/:conversation/attachments/:attachment. The
// audience is the conversation's read rule.
func (s *Service) getConversationAttachment(c echo.Context) error {
	attachment, content, err := s.readAttachment(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), c.Param("attachment"))
	if err != nil {
		return s.nativeAPIError(c, err)
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
func (s *Service) deleteConversationAttachment(c echo.Context) error {
	_, err := s.commandDeleteAttachment(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), c.Param("attachment"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) archiveConversation(c echo.Context) error {
	scope := nativeRequestScope(c)
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	id := c.Param("conversation")
	original, err := s.conversations.ReadPublicationConversationForOrganization(ctx, scope.organization, id)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeConversationRemoval(ctx, r, scope, original)
	}); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := requireConversationRemoval(original); err != nil {
		return s.nativeAPIError(c, err)
	}
	record := original
	record.Archived, record.Revision, record.EventSeq, record.UpdatedAt = true, original.Revision+1, original.EventSeq+1, s.config.now()
	raw, err := json.Marshal(projectConversation(record))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ref, err := s.conversations.Store.PublishPayload(ctx, string(scope.organization), payload)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	contentID := contentArchiveID(string(scope.organization), ref)
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.LockConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationRemoval(ctx, w, scope, conversationRecordFromStore(stored)); err != nil {
			return err
		}
		if stored.Revision != original.Revision || stored.EventSequence != original.EventSeq {
			return nativeConflict(tracker.Revision(stored.Revision))
		}
		if err := requireConversationRemoval(conversationRecordFromStore(stored)); err != nil {
			return err
		}
		stored.Execution, stored.Preferences = original.Execution, original.Preferences
		stored.Archived, stored.UpdatedAt = true, record.UpdatedAt
		if _, err := w.UpdateConversation(ctx, stored, stored.Revision); err != nil {
			return err
		}
		if err := w.PutConversationContent(ctx, conversationstore.ContentObject{OrganizationID: scope.organization, ID: contentID, Reference: ref, CreatedAt: record.UpdatedAt}); err != nil {
			return err
		}
		event, err := w.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: scope.organization, ConversationID: id, ContentReference: contentID, Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{}`), CreatedAt: record.UpdatedAt}})
		record.EventSeq = event.Seq
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, translateConversationError(err))
	}
	s.conversations.Committed(record)
	return c.JSON(http.StatusOK, projectConversation(record))
}

func (s *Service) deleteConversation(c echo.Context) error {
	scope := nativeRequestScope(c)
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	id := c.Param("conversation")
	original, err := s.conversations.ReadPublicationConversationForOrganization(ctx, scope.organization, id)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.LockConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		record := conversationRecordFromStore(stored)
		if err := s.authorizeConversationRemoval(ctx, w, scope, record); err != nil {
			return err
		}
		if stored.Revision != original.Revision || stored.EventSequence != original.EventSeq {
			return nativeConflict(tracker.Revision(stored.Revision))
		}
		if err := requireConversationRemoval(record); err != nil {
			return err
		}
		if err := requireConversationRemoval(original); err != nil {
			return err
		}
		owner, ok := w.(conversationstore.RemovalWriter)
		if !ok {
			return errors.New("conversation removal owner is required")
		}
		return owner.DeleteConversation(ctx, scope.project, id, s.config.now())
	})
	if err != nil {
		return s.nativeAPIError(c, translateConversationError(err))
	}
	s.conversations.Broker.Notify(id)
	return c.NoContent(http.StatusNoContent)
}

// listWorkItemReferences implements GET /work-items/:item/references. The
// scope middleware has already proved the reader can read the project; the
// issue read proves the issue exists in it, and the conversation read rule
// drops entries from private chats the reader does not own.
func (s *Service) listWorkItemReferences(c echo.Context) error {
	value, err := s.readWorkItemReferences(c.Request().Context(), nativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// registerConversationRoutes mounts the API, the event stream and the
// client shell. The individual groups are implemented in sibling files.
func (s *Service) registerConversationRoutes(e *echo.Echo) {
	if s.conversations == nil {
		return
	}
	s.registerConversationAPIRoutes(e)
	s.registerConversationWorkerRoutes(e)
}
