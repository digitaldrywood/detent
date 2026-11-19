package coreimport

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type LocalBoardDeps struct {
	Repository                    tenantstore.Repository
	Clock                         func() time.Time
	Plans                         func() *hostedplans.Service
	InvalidAPIRequest             func(echo.Context, error) error
	NativeAPIError                func(echo.Context, error, ...any) error
	AuthorizeWorkItemRecordScope  func(context.Context, accessstore.Reader, hubscope.NativeScope) error
	HydrateWorkItemRecord         func(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	PlanWorkItemRecord            func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan, any) (hubrecords.WorkItemRecordPlan, error)
	PrepareWorkItemFields         func(context.Context, tracker.OrganizationID, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error)
	RunDetailStore                func() attachment.DetailStore
	RecheckProjectRecordMutation  func(context.Context, hubscope.NativeScope) error
	BeginProjectRecordAuthority   func(context.Context, hubscope.NativeScope) (interface{ Rollback() error }, error)
	RecordEntryAccessKeyAuditWith func(context.Context, tenantstore.Writer, *hubscope.EntryKeyAudit, int) error
	ValidateWorkItemCreate        func(context.Context, hubscope.NativeScope, projectstore.Record, tracker.CreateIssue) error
	ValidateLinkedRepository      func(projectstore.Record, string) error
	IsRunnerBudget                func(context.Context) bool
	NotifyBoard                   func()
}

func (s LocalBoardDeps) ImportLocalBoard(c echo.Context) error {
	var request localboard.Request
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		var oversized *http.MaxBytesError
		if !errors.As(err, &oversized) {
			err = errors.New("invalid local board request")
		}
		return s.InvalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	if err := localboard.Validate(request.Record); err != nil {
		return s.NativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	if err := hubrecords.ValidateNativeContent(request.Record.Title, request.Record.Body, nil, nil, request.Record.Priority); err != nil {
		return s.NativeAPIError(c, err)
	}
	result, err := s.ImportLocalBoardRecord(c.Request().Context(), scope, request)
	if err != nil {
		return s.NativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func localBoardState(states []tracker.NativeState, source string) (string, bool, error) {
	for _, state := range states {
		if strings.EqualFold(strings.TrimSpace(source), state.Name) {
			return state.Name, false, nil
		}
	}
	for _, name := range []string{"Backlog", "Triage", ""} {
		for _, state := range states {
			if !state.Dispatchable && !state.Terminal && (name == "" || state.Name == name) {
				return state.Name, true, nil
			}
		}
	}
	return "", false, hubapi.NativePublicInvalid("Local board import requires a nondispatchable nonterminal fallback state")
}
