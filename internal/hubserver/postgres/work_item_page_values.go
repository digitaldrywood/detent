package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const itemPageColumns = `organization_id,project_id,work_item_id,number,revision,profile,web_url,issue_type,title,body,state,reporter_status,archived,terminal,scheduling_eligible,priority,labels,assignees,created_at,updated_at,closed_at,last_activity_at,ignore_dependencies,actor_kind,actor_principal_id,actor_via_key,actor_display_name,actor_agent,provenance_provider,provenance_external_id,provenance_author_id,provenance_author_display_name,provenance_created_at,provenance_updated_at,provenance_observed_at,report_url,report_app_build,report_browser,report_os,report_viewport,report_captured_at,verification_state,verification_at,verifier_kind,verifier_principal_id,verifier_via_key,verifier_display_name,verifier_agent,contract_exempt,contract_human_action,contract_return_state,contract_recorded_at,contract_release_policy,contract_release_body,release_source_issue_id,release_source_change_id,release_source_merge_sha,release_repository,release_pr_number,release_head_ref,release_head_sha,release_base_ref,release_base_sha,release_frozen_sha,release_change_id,release_version_id,release_associated_at,release_actor_kind,release_actor_principal_id,release_actor_via_key,release_actor_display_name,release_actor_agent,source_url,source_status,has_provenance,has_reportcontext,has_verification,has_verification_verifiedby,has_issuecontract,has_issuecontract_releasedelivery,has_releasedelivery,has_linkedsource,source_generation,source_at,projected_at,contract_section_names,contract_section_values,reference_providers,reference_kinds,reference_ids,reference_urls,reference_repositories,reference_numbers`

func scanItemPage(row interface{ Scan(...any) error }) (viewstore.ItemPageRecord, error) {
	var record viewstore.ItemPageRecord
	value := &record.Value
	value.Provenance = &tracker.Provenance{}
	var hasProvenance bool
	value.ReportContext = &tracker.ReportContext{}
	var hasReportContext bool
	value.Verification = &tracker.NativeVerification{}
	var hasVerification bool
	value.Verification.VerifiedBy = &tracker.Actor{}
	var hasVerificationVerifiedBy bool
	value.IssueContract = &issuecontract.State{}
	var hasIssueContract bool
	value.IssueContract.ReleaseDelivery = &issuecontract.ReleaseDeliveryApproval{}
	var hasIssueContractReleaseDelivery bool
	value.ReleaseDelivery = &tracker.ReleaseDelivery{}
	var hasReleaseDelivery bool
	value.LinkedSource = &tracker.LinkedIssueSource{}
	var hasLinkedSource bool
	var sectionNames, sectionValues, providers, kinds, ids, urls, repositories []string
	var numbers []int
	err := row.Scan(&value.OrganizationID, &value.ProjectID, &value.WorkItemID, &value.Number, &value.Revision, &value.Profile, &value.WebURL, &value.Type, &value.Title, &value.Body, &value.State, &value.ReporterStatus, &value.Archived, &value.Terminal, &value.SchedulingEligible, &value.Priority, &value.Labels, &value.Assignees, &value.CreatedAt, &value.UpdatedAt, &value.ClosedAt, &value.LastActivityAt, &value.IgnoreDependencies, &value.Actor.Kind, &value.Actor.PrincipalID, &value.Actor.ViaKey, &value.Actor.DisplayName, &value.Actor.Agent, &value.Provenance.Provider, &value.Provenance.ExternalID, &value.Provenance.AuthorID, &value.Provenance.AuthorDisplayName, &value.Provenance.CreatedAt, &value.Provenance.UpdatedAt, &value.Provenance.ObservedAt, &value.ReportContext.URL, &value.ReportContext.AppBuild, &value.ReportContext.Browser, &value.ReportContext.OS, &value.ReportContext.Viewport, &value.ReportContext.CapturedAt, &value.Verification.State, &value.Verification.VerifiedAt, &value.Verification.VerifiedBy.Kind, &value.Verification.VerifiedBy.PrincipalID, &value.Verification.VerifiedBy.ViaKey, &value.Verification.VerifiedBy.DisplayName, &value.Verification.VerifiedBy.Agent, &value.IssueContract.Exempt, &value.IssueContract.HumanAction, &value.IssueContract.ReturnState, &value.IssueContract.RecordedAt, &value.IssueContract.ReleaseDelivery.PolicyID, &value.IssueContract.ReleaseDelivery.BodySHA256, &value.ReleaseDelivery.SourceIssueID, &value.ReleaseDelivery.SourceChangeID, &value.ReleaseDelivery.SourceMergeSHA, &value.ReleaseDelivery.Repository, &value.ReleaseDelivery.PRNumber, &value.ReleaseDelivery.HeadRef, &value.ReleaseDelivery.HeadSHA, &value.ReleaseDelivery.BaseRef, &value.ReleaseDelivery.BaseSHA, &value.ReleaseDelivery.FrozenSHA, &value.ReleaseDelivery.ChangeID, &value.ReleaseDelivery.VersionID, &value.ReleaseDelivery.AssociatedAt, &value.ReleaseDelivery.Actor.Kind, &value.ReleaseDelivery.Actor.PrincipalID, &value.ReleaseDelivery.Actor.ViaKey, &value.ReleaseDelivery.Actor.DisplayName, &value.ReleaseDelivery.Actor.Agent, &value.LinkedSource.URL, &value.LinkedSource.Status, &hasProvenance, &hasReportContext, &hasVerification, &hasVerificationVerifiedBy, &hasIssueContract, &hasIssueContractReleaseDelivery, &hasReleaseDelivery, &hasLinkedSource, &record.Generation, &record.SourceAt, &record.ProjectedAt, &sectionNames, &sectionValues, &providers, &kinds, &ids, &urls, &repositories, &numbers)
	if err != nil {
		return record, err
	}
	value.IssueContract.ConfirmedSections = map[string]string{}
	for i, key := range sectionNames {
		value.IssueContract.ConfirmedSections[key] = sectionValues[i]
	}
	for i, provider := range providers {
		value.ExternalReferences = append(value.ExternalReferences, tracker.ExternalReference{Provider: provider, Kind: kinds[i], ID: ids[i], URL: urls[i], Repository: repositories[i], Number: numbers[i]})
	}
	value.OmittedFields = []string{"change_sequence", "dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason", "linked_source.snapshot"}
	value.Dependencies = []tracker.NativeWorkItemID{}
	value.Blockers = []tracker.NativeDependency{}
	value.Dependents = []tracker.NativeDependency{}
	if !hasLinkedSource {
		value.LinkedSource = nil
	}
	if !hasReleaseDelivery {
		value.ReleaseDelivery = nil
	}
	if !hasIssueContractReleaseDelivery {
		value.IssueContract.ReleaseDelivery = nil
	}
	if !hasIssueContract {
		value.IssueContract = nil
	}
	if !hasVerificationVerifiedBy {
		value.Verification.VerifiedBy = nil
	}
	if !hasVerification {
		value.Verification = nil
	}
	if !hasReportContext {
		value.ReportContext = nil
	}
	if !hasProvenance {
		value.Provenance = nil
	}
	return record, nil
}
func (s *session) putItemPage(ctx context.Context, record viewstore.ItemPageRecord) error {
	value := record.Value
	hasProvenance := value.Provenance != nil
	if hasProvenance {
		copy := *value.Provenance
		value.Provenance = &copy
	}
	if !hasProvenance {
		value.Provenance = &tracker.Provenance{}
	}
	hasReportContext := value.ReportContext != nil
	if hasReportContext {
		copy := *value.ReportContext
		value.ReportContext = &copy
	}
	if !hasReportContext {
		value.ReportContext = &tracker.ReportContext{}
	}
	hasVerification := value.Verification != nil
	if hasVerification {
		copy := *value.Verification
		value.Verification = &copy
	}
	if !hasVerification {
		value.Verification = &tracker.NativeVerification{}
	}
	hasVerificationVerifiedBy := value.Verification.VerifiedBy != nil
	if hasVerificationVerifiedBy {
		copy := *value.Verification.VerifiedBy
		value.Verification.VerifiedBy = &copy
	}
	if !hasVerificationVerifiedBy {
		value.Verification.VerifiedBy = &tracker.Actor{}
	}
	hasIssueContract := value.IssueContract != nil
	if hasIssueContract {
		copy := *value.IssueContract
		value.IssueContract = &copy
	}
	if !hasIssueContract {
		value.IssueContract = &issuecontract.State{}
	}
	hasIssueContractReleaseDelivery := value.IssueContract.ReleaseDelivery != nil
	if hasIssueContractReleaseDelivery {
		copy := *value.IssueContract.ReleaseDelivery
		value.IssueContract.ReleaseDelivery = &copy
	}
	if !hasIssueContractReleaseDelivery {
		value.IssueContract.ReleaseDelivery = &issuecontract.ReleaseDeliveryApproval{}
	}
	hasReleaseDelivery := value.ReleaseDelivery != nil
	if hasReleaseDelivery {
		copy := *value.ReleaseDelivery
		value.ReleaseDelivery = &copy
	}
	if !hasReleaseDelivery {
		value.ReleaseDelivery = &tracker.ReleaseDelivery{}
	}
	hasLinkedSource := value.LinkedSource != nil
	if hasLinkedSource {
		copy := *value.LinkedSource
		value.LinkedSource = &copy
	}
	if !hasLinkedSource {
		value.LinkedSource = &tracker.LinkedIssueSource{}
	}
	sectionNames, sectionValues := []string{}, []string{}
	for key, value := range value.IssueContract.ConfirmedSections {
		sectionNames = append(sectionNames, key)
		sectionValues = append(sectionValues, value)
	}
	providers, kinds, ids, urls, repositories, numbers := []string{}, []string{}, []string{}, []string{}, []string{}, []int{}
	for _, ref := range value.ExternalReferences {
		providers = append(providers, ref.Provider)
		kinds = append(kinds, ref.Kind)
		ids = append(ids, ref.ID)
		urls = append(urls, ref.URL)
		repositories = append(repositories, ref.Repository)
		numbers = append(numbers, ref.Number)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO native_work_item_pages (`+itemPageColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43,$44,$45,$46,$47,$48,$49,$50,$51,$52,$53,$54,$55,$56,$57,$58,$59,$60,$61,$62,$63,$64,$65,$66,$67,$68,$69,$70,$71,$72,$73,$74,$75,$76,$77,$78,$79,$80,$81,$82,$83,$84,$85,$86,$87,$88,$89,$90,$91,$92,$93) ON CONFLICT (organization_id,project_id,work_item_id) DO UPDATE SET number=EXCLUDED.number,revision=EXCLUDED.revision,profile=EXCLUDED.profile,web_url=EXCLUDED.web_url,issue_type=EXCLUDED.issue_type,title=EXCLUDED.title,body=EXCLUDED.body,state=EXCLUDED.state,reporter_status=EXCLUDED.reporter_status,archived=EXCLUDED.archived,terminal=EXCLUDED.terminal,scheduling_eligible=EXCLUDED.scheduling_eligible,priority=EXCLUDED.priority,labels=EXCLUDED.labels,assignees=EXCLUDED.assignees,created_at=EXCLUDED.created_at,updated_at=EXCLUDED.updated_at,closed_at=EXCLUDED.closed_at,last_activity_at=EXCLUDED.last_activity_at,ignore_dependencies=EXCLUDED.ignore_dependencies,actor_kind=EXCLUDED.actor_kind,actor_principal_id=EXCLUDED.actor_principal_id,actor_via_key=EXCLUDED.actor_via_key,actor_display_name=EXCLUDED.actor_display_name,actor_agent=EXCLUDED.actor_agent,provenance_provider=EXCLUDED.provenance_provider,provenance_external_id=EXCLUDED.provenance_external_id,provenance_author_id=EXCLUDED.provenance_author_id,provenance_author_display_name=EXCLUDED.provenance_author_display_name,provenance_created_at=EXCLUDED.provenance_created_at,provenance_updated_at=EXCLUDED.provenance_updated_at,provenance_observed_at=EXCLUDED.provenance_observed_at,report_url=EXCLUDED.report_url,report_app_build=EXCLUDED.report_app_build,report_browser=EXCLUDED.report_browser,report_os=EXCLUDED.report_os,report_viewport=EXCLUDED.report_viewport,report_captured_at=EXCLUDED.report_captured_at,verification_state=EXCLUDED.verification_state,verification_at=EXCLUDED.verification_at,verifier_kind=EXCLUDED.verifier_kind,verifier_principal_id=EXCLUDED.verifier_principal_id,verifier_via_key=EXCLUDED.verifier_via_key,verifier_display_name=EXCLUDED.verifier_display_name,verifier_agent=EXCLUDED.verifier_agent,contract_exempt=EXCLUDED.contract_exempt,contract_human_action=EXCLUDED.contract_human_action,contract_return_state=EXCLUDED.contract_return_state,contract_recorded_at=EXCLUDED.contract_recorded_at,contract_release_policy=EXCLUDED.contract_release_policy,contract_release_body=EXCLUDED.contract_release_body,release_source_issue_id=EXCLUDED.release_source_issue_id,release_source_change_id=EXCLUDED.release_source_change_id,release_source_merge_sha=EXCLUDED.release_source_merge_sha,release_repository=EXCLUDED.release_repository,release_pr_number=EXCLUDED.release_pr_number,release_head_ref=EXCLUDED.release_head_ref,release_head_sha=EXCLUDED.release_head_sha,release_base_ref=EXCLUDED.release_base_ref,release_base_sha=EXCLUDED.release_base_sha,release_frozen_sha=EXCLUDED.release_frozen_sha,release_change_id=EXCLUDED.release_change_id,release_version_id=EXCLUDED.release_version_id,release_associated_at=EXCLUDED.release_associated_at,release_actor_kind=EXCLUDED.release_actor_kind,release_actor_principal_id=EXCLUDED.release_actor_principal_id,release_actor_via_key=EXCLUDED.release_actor_via_key,release_actor_display_name=EXCLUDED.release_actor_display_name,release_actor_agent=EXCLUDED.release_actor_agent,source_url=EXCLUDED.source_url,source_status=EXCLUDED.source_status,has_provenance=EXCLUDED.has_provenance,has_reportcontext=EXCLUDED.has_reportcontext,has_verification=EXCLUDED.has_verification,has_verification_verifiedby=EXCLUDED.has_verification_verifiedby,has_issuecontract=EXCLUDED.has_issuecontract,has_issuecontract_releasedelivery=EXCLUDED.has_issuecontract_releasedelivery,has_releasedelivery=EXCLUDED.has_releasedelivery,has_linkedsource=EXCLUDED.has_linkedsource,source_generation=EXCLUDED.source_generation,source_at=EXCLUDED.source_at,projected_at=EXCLUDED.projected_at,contract_section_names=EXCLUDED.contract_section_names,contract_section_values=EXCLUDED.contract_section_values,reference_providers=EXCLUDED.reference_providers,reference_kinds=EXCLUDED.reference_kinds,reference_ids=EXCLUDED.reference_ids,reference_urls=EXCLUDED.reference_urls,reference_repositories=EXCLUDED.reference_repositories,reference_numbers=EXCLUDED.reference_numbers WHERE native_work_item_pages.source_generation<=EXCLUDED.source_generation`, value.OrganizationID, value.ProjectID, value.WorkItemID, value.Number, value.Revision, value.Profile, value.WebURL, value.Type, value.Title, value.Body, value.State, value.ReporterStatus, value.Archived, value.Terminal, value.SchedulingEligible, value.Priority, value.Labels, value.Assignees, value.CreatedAt, value.UpdatedAt, value.ClosedAt, value.LastActivityAt, value.IgnoreDependencies, value.Actor.Kind, value.Actor.PrincipalID, value.Actor.ViaKey, value.Actor.DisplayName, value.Actor.Agent, value.Provenance.Provider, value.Provenance.ExternalID, value.Provenance.AuthorID, value.Provenance.AuthorDisplayName, value.Provenance.CreatedAt, value.Provenance.UpdatedAt, value.Provenance.ObservedAt, value.ReportContext.URL, value.ReportContext.AppBuild, value.ReportContext.Browser, value.ReportContext.OS, value.ReportContext.Viewport, value.ReportContext.CapturedAt, value.Verification.State, value.Verification.VerifiedAt, value.Verification.VerifiedBy.Kind, value.Verification.VerifiedBy.PrincipalID, value.Verification.VerifiedBy.ViaKey, value.Verification.VerifiedBy.DisplayName, value.Verification.VerifiedBy.Agent, value.IssueContract.Exempt, value.IssueContract.HumanAction, value.IssueContract.ReturnState, value.IssueContract.RecordedAt, value.IssueContract.ReleaseDelivery.PolicyID, value.IssueContract.ReleaseDelivery.BodySHA256, value.ReleaseDelivery.SourceIssueID, value.ReleaseDelivery.SourceChangeID, value.ReleaseDelivery.SourceMergeSHA, value.ReleaseDelivery.Repository, value.ReleaseDelivery.PRNumber, value.ReleaseDelivery.HeadRef, value.ReleaseDelivery.HeadSHA, value.ReleaseDelivery.BaseRef, value.ReleaseDelivery.BaseSHA, value.ReleaseDelivery.FrozenSHA, value.ReleaseDelivery.ChangeID, value.ReleaseDelivery.VersionID, value.ReleaseDelivery.AssociatedAt, value.ReleaseDelivery.Actor.Kind, value.ReleaseDelivery.Actor.PrincipalID, value.ReleaseDelivery.Actor.ViaKey, value.ReleaseDelivery.Actor.DisplayName, value.ReleaseDelivery.Actor.Agent, value.LinkedSource.URL, value.LinkedSource.Status, hasProvenance, hasReportContext, hasVerification, hasVerificationVerifiedBy, hasIssueContract, hasIssueContractReleaseDelivery, hasReleaseDelivery, hasLinkedSource, record.Generation, record.SourceAt, record.ProjectedAt, sectionNames, sectionValues, providers, kinds, ids, urls, repositories, numbers)
	return mapError(ctx, err)
}

const commentPageColumns = `organization_id,project_id,work_item_id,comment_id,revision,sequence,body,deleted,created_at,updated_at,actor_kind,actor_principal_id,actor_via_key,actor_display_name,actor_agent,editor_kind,editor_principal_id,editor_via_key,editor_display_name,editor_agent,provenance_provider,provenance_external_id,provenance_author_id,provenance_author_display_name,provenance_created_at,provenance_updated_at,provenance_observed_at,has_editedby,has_provenance,source_generation,source_at,projected_at`

func scanCommentPage(row interface{ Scan(...any) error }) (viewstore.CommentPageRecord, error) {
	var record viewstore.CommentPageRecord
	value := &record.Value
	value.EditedBy = &tracker.Actor{}
	var hasEditedBy bool
	value.Provenance = &tracker.Provenance{}
	var hasProvenance bool
	err := row.Scan(&value.OrganizationID, &value.ProjectID, &value.WorkItemID, &value.ID, &value.Revision, &value.Sequence, &value.Body, &value.Deleted, &value.CreatedAt, &value.UpdatedAt, &value.Actor.Kind, &value.Actor.PrincipalID, &value.Actor.ViaKey, &value.Actor.DisplayName, &value.Actor.Agent, &value.EditedBy.Kind, &value.EditedBy.PrincipalID, &value.EditedBy.ViaKey, &value.EditedBy.DisplayName, &value.EditedBy.Agent, &value.Provenance.Provider, &value.Provenance.ExternalID, &value.Provenance.AuthorID, &value.Provenance.AuthorDisplayName, &value.Provenance.CreatedAt, &value.Provenance.UpdatedAt, &value.Provenance.ObservedAt, &hasEditedBy, &hasProvenance, &record.Generation, &record.SourceAt, &record.ProjectedAt)
	if err != nil {
		return record, err
	}
	if !hasProvenance {
		value.Provenance = nil
	}
	if !hasEditedBy {
		value.EditedBy = nil
	}
	return record, nil
}
func (s *session) putCommentPage(ctx context.Context, record viewstore.CommentPageRecord) error {
	value := record.Value
	hasEditedBy := value.EditedBy != nil
	if hasEditedBy {
		copy := *value.EditedBy
		value.EditedBy = &copy
	}
	if !hasEditedBy {
		value.EditedBy = &tracker.Actor{}
	}
	hasProvenance := value.Provenance != nil
	if hasProvenance {
		copy := *value.Provenance
		value.Provenance = &copy
	}
	if !hasProvenance {
		value.Provenance = &tracker.Provenance{}
	}
	_, err := s.db.Exec(ctx, `INSERT INTO native_comment_pages (`+commentPageColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32) ON CONFLICT (organization_id,project_id,work_item_id,comment_id) DO UPDATE SET revision=EXCLUDED.revision,sequence=EXCLUDED.sequence,body=EXCLUDED.body,deleted=EXCLUDED.deleted,created_at=EXCLUDED.created_at,updated_at=EXCLUDED.updated_at,actor_kind=EXCLUDED.actor_kind,actor_principal_id=EXCLUDED.actor_principal_id,actor_via_key=EXCLUDED.actor_via_key,actor_display_name=EXCLUDED.actor_display_name,actor_agent=EXCLUDED.actor_agent,editor_kind=EXCLUDED.editor_kind,editor_principal_id=EXCLUDED.editor_principal_id,editor_via_key=EXCLUDED.editor_via_key,editor_display_name=EXCLUDED.editor_display_name,editor_agent=EXCLUDED.editor_agent,provenance_provider=EXCLUDED.provenance_provider,provenance_external_id=EXCLUDED.provenance_external_id,provenance_author_id=EXCLUDED.provenance_author_id,provenance_author_display_name=EXCLUDED.provenance_author_display_name,provenance_created_at=EXCLUDED.provenance_created_at,provenance_updated_at=EXCLUDED.provenance_updated_at,provenance_observed_at=EXCLUDED.provenance_observed_at,has_editedby=EXCLUDED.has_editedby,has_provenance=EXCLUDED.has_provenance,source_generation=EXCLUDED.source_generation,source_at=EXCLUDED.source_at,projected_at=EXCLUDED.projected_at WHERE native_comment_pages.source_generation<=EXCLUDED.source_generation`, value.OrganizationID, value.ProjectID, value.WorkItemID, value.ID, value.Revision, value.Sequence, value.Body, value.Deleted, value.CreatedAt, value.UpdatedAt, value.Actor.Kind, value.Actor.PrincipalID, value.Actor.ViaKey, value.Actor.DisplayName, value.Actor.Agent, value.EditedBy.Kind, value.EditedBy.PrincipalID, value.EditedBy.ViaKey, value.EditedBy.DisplayName, value.EditedBy.Agent, value.Provenance.Provider, value.Provenance.ExternalID, value.Provenance.AuthorID, value.Provenance.AuthorDisplayName, value.Provenance.CreatedAt, value.Provenance.UpdatedAt, value.Provenance.ObservedAt, hasEditedBy, hasProvenance, record.Generation, record.SourceAt, record.ProjectedAt)
	return mapError(ctx, err)
}
