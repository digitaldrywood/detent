package postgres

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const attemptIdentitySelect = `identity_sequence,identity_execution_role,identity_execution_backend,identity_execution_model,identity_execution_reasoning_effort,identity_selection_effort_clamped_from,identity_selection_policy,identity_selection_policy_source,identity_selection_reason,identity_selection_level,identity_selection_requested_model,identity_selection_model_source,identity_selection_effort_source,identity_selection_fallback_reason,identity_selection_backend_source,identity_selection_route_source,identity_backend_id,identity_backend_kind,identity_route,identity_role,identity_provider,identity_provider_provenance,identity_requested_model,identity_requested_model_provenance,identity_resolved_model,identity_resolved_model_provenance,identity_reasoning_effort,identity_reasoning_effort_provenance,identity_service_tier,identity_service_tier_provenance,identity_observed_at`

const attemptIdentitySelectAliased = `a.identity_sequence,a.identity_execution_role,a.identity_execution_backend,a.identity_execution_model,a.identity_execution_reasoning_effort,a.identity_selection_effort_clamped_from,a.identity_selection_policy,a.identity_selection_policy_source,a.identity_selection_reason,a.identity_selection_level,a.identity_selection_requested_model,a.identity_selection_model_source,a.identity_selection_effort_source,a.identity_selection_fallback_reason,a.identity_selection_backend_source,a.identity_selection_route_source,a.identity_backend_id,a.identity_backend_kind,a.identity_route,a.identity_role,a.identity_provider,a.identity_provider_provenance,a.identity_requested_model,a.identity_requested_model_provenance,a.identity_resolved_model,a.identity_resolved_model_provenance,a.identity_reasoning_effort,a.identity_reasoning_effort_provenance,a.identity_service_tier,a.identity_service_tier_provenance,a.identity_observed_at`

type attemptIdentityValue struct {
	value      *string
	provenance *string
}

type attemptIdentityColumns struct {
	executionRole              *string
	executionBackend           *string
	executionModel             *string
	executionReasoningEffort   *string
	selectionPresent           bool
	selectionEffortClampedFrom *string
	selectionPolicy            *string
	selectionPolicySource      *string
	selectionReason            *string
	selectionLevel             *string
	selectionRequestedModel    *string
	selectionModelSource       *string
	selectionEffortSource      *string
	selectionFallbackReason    *string
	selectionBackendSource     *string
	selectionRouteSource       *string
	backendID                  *string
	backendKind                *string
	route                      *string
	role                       *string
	provider                   attemptIdentityValue
	requestedModel             attemptIdentityValue
	resolvedModel              attemptIdentityValue
	reasoningEffort            attemptIdentityValue
	serviceTier                attemptIdentityValue
	observedAt                 *time.Time
}

func newAttemptIdentityColumns(identity leasestore.AttemptIdentity) attemptIdentityColumns {
	var columns attemptIdentityColumns
	if e := identity.Execution; e != nil {
		columns.executionRole, columns.executionBackend, columns.executionModel, columns.executionReasoningEffort = new(e.Role), new(e.Backend), new(e.Model), identityText(e.ReasoningEffort)
	}
	runtime := identity.Runtime.Normalize()
	selection := runtime.Selection
	columns.selectionPresent = selection != (agentidentity.Selection{})
	columns.selectionEffortClampedFrom = identityText(selection.EffortClampedFrom)
	columns.selectionPolicy = identityText(selection.Policy)
	columns.selectionPolicySource = identityText(selection.PolicySource)
	columns.selectionReason = identityText(selection.Reason)
	columns.selectionLevel = identityText(selection.Level)
	columns.selectionRequestedModel = identityText(selection.RequestedModel)
	columns.selectionModelSource = identityText(selection.ModelSource)
	columns.selectionEffortSource = identityText(selection.EffortSource)
	columns.selectionFallbackReason = identityText(selection.FallbackReason)
	columns.selectionBackendSource = identityText(selection.BackendSource)
	columns.selectionRouteSource = identityText(selection.RouteSource)
	columns.backendID = identityText(runtime.BackendID)
	columns.backendKind = identityText(runtime.BackendKind)
	columns.route = identityText(runtime.Route)
	columns.role = identityText(runtime.Role)
	columns.provider = newAttemptIdentityValue(runtime.Provider)
	columns.requestedModel = newAttemptIdentityValue(runtime.RequestedModel)
	columns.resolvedModel = newAttemptIdentityValue(runtime.ResolvedModel)
	columns.reasoningEffort = newAttemptIdentityValue(runtime.ReasoningEffort)
	columns.serviceTier = newAttemptIdentityValue(runtime.ServiceTier)
	if runtime.ObservedAt != nil {
		columns.observedAt = new(runtime.ObservedAt.UTC().Truncate(time.Microsecond))
	}
	return columns
}

func newAttemptIdentityValue(value agentidentity.Value) attemptIdentityValue {
	return attemptIdentityValue{value: identityText(value.Value), provenance: identityText(string(value.Provenance))}
}

func identityText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func identityString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (v attemptIdentityValue) identity() agentidentity.Value {
	return agentidentity.Value{Value: identityString(v.value), Provenance: agentidentity.Provenance(identityString(v.provenance))}
}

func (c attemptIdentityColumns) insert(p *pgquery.InsertLeaseAttemptParams) {
	p.IdentityExecutionRole, p.IdentityExecutionBackend, p.IdentityExecutionModel, p.IdentityExecutionReasoningEffort = c.executionRole, c.executionBackend, c.executionModel, c.executionReasoningEffort
	p.IdentitySelectionEffortClampedFrom, p.IdentitySelectionPolicy, p.IdentitySelectionPolicySource, p.IdentitySelectionReason = c.selectionEffortClampedFrom, c.selectionPolicy, c.selectionPolicySource, c.selectionReason
	p.IdentitySelectionLevel, p.IdentitySelectionRequestedModel, p.IdentitySelectionModelSource, p.IdentitySelectionEffortSource = c.selectionLevel, c.selectionRequestedModel, c.selectionModelSource, c.selectionEffortSource
	p.IdentitySelectionFallbackReason, p.IdentitySelectionBackendSource, p.IdentitySelectionRouteSource = c.selectionFallbackReason, c.selectionBackendSource, c.selectionRouteSource
	p.IdentityBackendID, p.IdentityBackendKind, p.IdentityRoute, p.IdentityRole = c.backendID, c.backendKind, c.route, c.role
	p.IdentityProvider, p.IdentityProviderProvenance = c.provider.value, c.provider.provenance
	p.IdentityRequestedModel, p.IdentityRequestedModelProvenance = c.requestedModel.value, c.requestedModel.provenance
	p.IdentityResolvedModel, p.IdentityResolvedModelProvenance = c.resolvedModel.value, c.resolvedModel.provenance
	p.IdentityReasoningEffort, p.IdentityReasoningEffortProvenance = c.reasoningEffort.value, c.reasoningEffort.provenance
	p.IdentityServiceTier, p.IdentityServiceTierProvenance = c.serviceTier.value, c.serviceTier.provenance
	p.IdentityObservedAt = c.observedAt
}

func (c attemptIdentityColumns) advance(p *pgquery.AdvanceLeaseAttemptParams) {
	p.IdentityExecutionRole, p.IdentityExecutionBackend, p.IdentityExecutionModel, p.IdentityExecutionReasoningEffort = c.executionRole, c.executionBackend, c.executionModel, c.executionReasoningEffort
	p.IdentitySelectionPresent = c.selectionPresent
	p.IdentitySelectionEffortClampedFrom, p.IdentitySelectionPolicy, p.IdentitySelectionPolicySource, p.IdentitySelectionReason = c.selectionEffortClampedFrom, c.selectionPolicy, c.selectionPolicySource, c.selectionReason
	p.IdentitySelectionLevel, p.IdentitySelectionRequestedModel, p.IdentitySelectionModelSource, p.IdentitySelectionEffortSource = c.selectionLevel, c.selectionRequestedModel, c.selectionModelSource, c.selectionEffortSource
	p.IdentitySelectionFallbackReason, p.IdentitySelectionBackendSource, p.IdentitySelectionRouteSource = c.selectionFallbackReason, c.selectionBackendSource, c.selectionRouteSource
	p.IdentityBackendID, p.IdentityBackendKind, p.IdentityRoute, p.IdentityRole = c.backendID, c.backendKind, c.route, c.role
	p.IdentityProvider, p.IdentityProviderProvenance = c.provider.value, c.provider.provenance
	p.IdentityRequestedModel, p.IdentityRequestedModelProvenance = c.requestedModel.value, c.requestedModel.provenance
	p.IdentityResolvedModel, p.IdentityResolvedModelProvenance = c.resolvedModel.value, c.resolvedModel.provenance
	p.IdentityReasoningEffort, p.IdentityReasoningEffortProvenance = c.reasoningEffort.value, c.reasoningEffort.provenance
	p.IdentityServiceTier, p.IdentityServiceTierProvenance = c.serviceTier.value, c.serviceTier.provenance
	p.IdentityObservedAt = c.observedAt
}

func (c attemptIdentityColumns) store(row *pgquery.NativeAttempt) {
	row.IdentityExecutionRole, row.IdentityExecutionBackend, row.IdentityExecutionModel, row.IdentityExecutionReasoningEffort = c.executionRole, c.executionBackend, c.executionModel, c.executionReasoningEffort
	row.IdentitySelectionEffortClampedFrom, row.IdentitySelectionPolicy, row.IdentitySelectionPolicySource, row.IdentitySelectionReason = c.selectionEffortClampedFrom, c.selectionPolicy, c.selectionPolicySource, c.selectionReason
	row.IdentitySelectionLevel, row.IdentitySelectionRequestedModel, row.IdentitySelectionModelSource, row.IdentitySelectionEffortSource = c.selectionLevel, c.selectionRequestedModel, c.selectionModelSource, c.selectionEffortSource
	row.IdentitySelectionFallbackReason, row.IdentitySelectionBackendSource, row.IdentitySelectionRouteSource = c.selectionFallbackReason, c.selectionBackendSource, c.selectionRouteSource
	row.IdentityBackendID, row.IdentityBackendKind, row.IdentityRoute, row.IdentityRole = c.backendID, c.backendKind, c.route, c.role
	row.IdentityProvider, row.IdentityProviderProvenance = c.provider.value, c.provider.provenance
	row.IdentityRequestedModel, row.IdentityRequestedModelProvenance = c.requestedModel.value, c.requestedModel.provenance
	row.IdentityResolvedModel, row.IdentityResolvedModelProvenance = c.resolvedModel.value, c.resolvedModel.provenance
	row.IdentityReasoningEffort, row.IdentityReasoningEffortProvenance = c.reasoningEffort.value, c.reasoningEffort.provenance
	row.IdentityServiceTier, row.IdentityServiceTierProvenance = c.serviceTier.value, c.serviceTier.provenance
	row.IdentityObservedAt = c.observedAt
}

func attemptIdentityFromPG(row pgquery.NativeAttempt) leasestore.AttemptIdentity {
	var identity leasestore.AttemptIdentity
	if row.IdentityExecutionRole != nil {
		identity.Execution = &tracker.NativeExecutionIdentity{Role: *row.IdentityExecutionRole, Backend: identityString(row.IdentityExecutionBackend), Model: identityString(row.IdentityExecutionModel), ReasoningEffort: identityString(row.IdentityExecutionReasoningEffort)}
	}
	identity.Runtime = agentidentity.Identity{
		Selection: agentidentity.Selection{
			EffortClampedFrom: identityString(row.IdentitySelectionEffortClampedFrom), Policy: identityString(row.IdentitySelectionPolicy),
			PolicySource: identityString(row.IdentitySelectionPolicySource), Reason: identityString(row.IdentitySelectionReason),
			Level: identityString(row.IdentitySelectionLevel), RequestedModel: identityString(row.IdentitySelectionRequestedModel),
			ModelSource: identityString(row.IdentitySelectionModelSource), EffortSource: identityString(row.IdentitySelectionEffortSource),
			FallbackReason: identityString(row.IdentitySelectionFallbackReason), BackendSource: identityString(row.IdentitySelectionBackendSource),
			RouteSource: identityString(row.IdentitySelectionRouteSource),
		},
		BackendID: identityString(row.IdentityBackendID), BackendKind: identityString(row.IdentityBackendKind),
		Route: identityString(row.IdentityRoute), Role: identityString(row.IdentityRole),
		Provider:        attemptIdentityValue{row.IdentityProvider, row.IdentityProviderProvenance}.identity(),
		RequestedModel:  attemptIdentityValue{row.IdentityRequestedModel, row.IdentityRequestedModelProvenance}.identity(),
		ResolvedModel:   attemptIdentityValue{row.IdentityResolvedModel, row.IdentityResolvedModelProvenance}.identity(),
		ReasoningEffort: attemptIdentityValue{row.IdentityReasoningEffort, row.IdentityReasoningEffortProvenance}.identity(),
		ServiceTier:     attemptIdentityValue{row.IdentityServiceTier, row.IdentityServiceTierProvenance}.identity(),
	}
	if row.IdentityObservedAt != nil {
		identity.Runtime.ObservedAt = new(row.IdentityObservedAt.UTC())
	}
	return identity
}

func mergeAttemptIdentityRow(row *pgquery.NativeAttempt, sequence int64, update leasestore.AttemptIdentity) {
	if sequence == 1 {
		newAttemptIdentityColumns(update).store(row)
		row.IdentitySequence = pgtype.Int8{Int64: 1, Valid: true}
		return
	}
	if !row.IdentitySequence.Valid || row.IdentitySequence.Int64 >= sequence {
		return
	}
	newAttemptIdentityColumns(attemptIdentityFromPG(*row).Merge(update)).store(row)
	row.IdentitySequence = pgtype.Int8{Int64: sequence, Valid: true}
}
