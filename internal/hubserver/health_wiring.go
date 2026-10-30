package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthDetector = hubhealth.Detector

func (s *Service) startHealthDetector() {
	s.healthDetector = hubhealth.StartDetector(s.evaluateTenantHealth, s.config.Logger)
}

func (s *Service) stopHealthDetector() {
	if s.healthDetector != nil {
		s.healthDetector.Stop()
	}
}

func (s *Service) evaluateTenantHealth(ctx context.Context) error {
	ctx = s.freshRunDetailContext(ctx)
	ctx, cancel := context.WithTimeout(s.runDetailContext(ctx), 45*time.Second)
	defer cancel()
	if s.requestMetrics != nil {
		if err := s.requestMetrics.flush(ctx, s.config.now()); err != nil {
			s.config.Logger.Warn("flush tenant request metrics", "error", err)
		}
	}
	return healthStoreFor(s.database.db, s.database.reader).EvaluateOrganizations(ctx, s.evaluateOrganizationHealth)
}

func (s *Service) evaluateOrganizationHealth(ctx context.Context, organization tracker.OrganizationID, now time.Time) error {
	snapshot, err := s.readHealthSnapshot(ctx, organization, now)
	if err != nil {
		return err
	}
	findings := hubhealth.Evaluate(now, snapshot)
	if s.requestMetrics != nil && organization == s.requestMetrics.organization {
		findings = append(findings, s.requestMetrics.findings(now)...)
	}
	return s.commitHealthEvaluation(ctx, organization, now, findings, snapshot.BaselineUnavailable)
}

func (s *Service) readHealthSnapshot(ctx context.Context, organization tracker.OrganizationID, now time.Time) (hubhealth.Snapshot, error) {
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return hubhealth.Snapshot{}, err
	}
	defer tx.Rollback()
	return readHealthSnapshotWithContracts(ctx, tx, organization, now, &s.healthDetector.Contracts)
}

func writeHealthEvaluation(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time, findings []hubhealth.Finding) ([]string, error) {
	return healthStoreFor(nil, nil).WriteEvaluation(ctx, tx, organization, now, findings)
}

func (s *Service) commitHealthEvaluation(ctx context.Context, organization tracker.OrganizationID, now time.Time, findings []hubhealth.Finding, unavailable []hubhealth.BaselineUnavailable) error {
	return healthEvaluationStore(s).CommitEvaluation(ctx, organization, now, findings, unavailable, s.config.Hosted == nil || s.config.Hosted.EmailSender == nil)
}

func (s *Service) sendHealthEmails(ctx context.Context, organization tracker.OrganizationID, findings []hubhealth.Finding) error {
	if string(organization) != s.config.Hosted.OrganizationID {
		return auth.ErrHostedIdentity
	}
	providerID, err := s.hostedProviderOrganization(ctx)
	if err != nil {
		return err
	}
	members, err := s.config.Hosted.Provider.Memberships(ctx, "", providerID)
	if err != nil {
		return err
	}
	recipients := []string{}
	for _, member := range members {
		if member.OrganizationID != providerID || member.Status != "active" || member.Role.Slug != "owner" {
			continue
		}
		user, err := auth.LookupHostedUser(ctx, s.config.Hosted.Provider, member.UserID)
		if err != nil {
			return err
		}
		recipients = append(recipients, strings.ToLower(strings.TrimSpace(user.Email)))
	}
	slices.Sort(recipients)
	recipients = slices.Compact(recipients)
	var failures error
	for _, f := range findings {
		message, err := s.renderHealthEmail(ctx, f)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		for _, recipient := range recipients {
			message.To = recipient
			if err := s.config.Hosted.EmailSender.SendEmail(ctx, message); err != nil {
				failures = errors.Join(failures, fmt.Errorf("send health finding %s email: %w", f.ID, err))
			}
		}
	}
	return failures
}

func (s *Service) renderHealthEmail(ctx context.Context, f hubhealth.Finding) (auth.EmailMessage, error) {

	link := s.config.Hosted.PublicURL + s.hostedPath("/fleet")
	if f.Subject.Kind == "work_item" {
		var err error
		link, err = s.issueBrowserURL(ctx, f.Subject.ID)
		if errors.Is(err, sql.ErrNoRows) {
			link = s.config.Hosted.PublicURL + s.hostedPath("/diagnostics")
		} else if err != nil {
			return auth.EmailMessage{}, err
		}
	}
	link, err := s.canonicalBrowserLink(ctx, link)
	if err != nil {
		return auth.EmailMessage{}, err
	}
	return hubhealth.RenderEmail(f, link)
}

func (s *Service) afterHealthEvaluation(ctx context.Context, organization tracker.OrganizationID, issues []tracker.NativeIssue) error {
	if s.outbox != nil {
		s.outbox.signal()
	}
	for _, issue := range issues {
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		s.wakeSpriteRunnersAfter(healthIssueScope(organization, issue.ProjectID), raw)
	}
	return nil
}

func healthIssueScope(organization tracker.OrganizationID, project tracker.ProjectID) nativeScope {
	return nativeScope{organization: organization, project: project, sourceActor: nativeIntegrationActor("health_detector")}
}

func reportHealthFindings(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time, cancellations map[string]hubhealth.Cancellation) ([]tracker.NativeIssue, error) {
	return healthStoreFor(nil, nil).ReportFindings(ctx, tx, organization, now, cancellations)
}

func reportHealthIssue(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.CreateIssue, subject hubhealth.Subject, linked tracker.NativeWorkItemID, opened, now time.Time, cancellations map[string]hubhealth.Cancellation) (tracker.NativeIssue, error) {
	search, err := prepareNativeDuplicates(ctx, tx, scope, request, now, true)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if len(search.matches) == 0 && linked != "" {
		var owner possibleDuplicate
		err := tx.QueryRowContext(ctx, `SELECT i.native_id,i.number,i.title,ws.detent_state,i.revision FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id
 WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND i.archived=0 AND ws.terminal=0`, scope.organization, scope.project, linked).Scan(&owner.WorkItemID, &owner.Number, &owner.Title, &owner.State, &owner.revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return tracker.NativeIssue{}, err
		}
		if err == nil {
			search.matches = []possibleDuplicate{owner}
		}
	}
	if (len(search.matches) == 0 || search.matches[0].terminal) && subject.Kind == "work_item" {
		var owner possibleDuplicate
		err := tx.QueryRowContext(ctx, `WITH subject AS (
 SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?
), blockers AS (
 SELECT blocker_issue_id AS id FROM issue_dependencies WHERE dependent_issue_id=(SELECT id FROM subject) LIMIT ?
), forward_links AS (
 SELECT second_issue_id AS id FROM issue_relationships WHERE first_issue_id=(SELECT id FROM subject) AND relationship_type='related' LIMIT ?
), reverse_links AS (
 SELECT first_issue_id AS id FROM issue_relationships WHERE second_issue_id=(SELECT id FROM subject) AND relationship_type='related' LIMIT ?
), candidates AS (SELECT id FROM blockers UNION SELECT id FROM forward_links UNION SELECT id FROM reverse_links)
 SELECT i.native_id,i.number,i.title,ws.detent_state,i.revision FROM candidates c JOIN issues i ON i.id=c.id
 JOIN workflow_states ws ON ws.id=i.workflow_state_id
 WHERE i.organization_id=? AND i.project_id=? AND i.archived=0 AND ws.terminal=0
 ORDER BY i.number DESC LIMIT 1`, scope.organization, scope.project, subject.ID, duplicateCandidateLimit, duplicateCandidateLimit, duplicateCandidateLimit, scope.organization, scope.project).Scan(&owner.WorkItemID, &owner.Number, &owner.Title, &owner.State, &owner.revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return tracker.NativeIssue{}, err
		}
		if err == nil {
			search.matches = []possibleDuplicate{owner}
		}
	}
	scope.duplicateSearch = search
	issue, err := reportNativeMachineOccurrenceTx(ctx, tx, scope, request, now, true)
	if err == nil && issue.Terminal && issue.State == "Done" && issue.Actor == scope.actor() && issue.Provenance == nil && hubhealth.LegacyIssueBody(issue.Body) {
		return createNativeIssue(ctx, tx, scope, request, now, true)
	}
	if err != nil || !issue.Terminal || issue.State != "Cancelled" || issue.Actor != scope.actor() {
		return issue, err
	}
	var id, actorRaw, dataRaw string
	var projected sql.NullInt64
	var automatic sql.NullBool
	var before bool
	err = tx.QueryRowContext(ctx, `SELECT id,health_projected,
 actor_kind='integration' AND actor_principal_id='health_detector'
 AND transition_from='Backlog' AND transition_to='Cancelled',
 julianday(recorded_at)<julianday(?),
 CASE WHEN health_projected IS NULL THEN actor_json ELSE '{}' END,
 CASE WHEN health_projected IS NULL THEN data_json ELSE '{}' END
 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned'
 ORDER BY sequence DESC LIMIT 1`, formatHubTime(opened), scope.organization, scope.project, issue.WorkItemID).Scan(&id, &projected, &automatic, &before, &actorRaw, &dataRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return issue, nil
	}
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	newEpisode := automatic.Valid && automatic.Bool && before
	if !projected.Valid {
		fact, ok := cancellations[id]
		if !ok || fact.ActorRaw != actorRaw || fact.DataRaw != dataRaw {
			return tracker.NativeIssue{}, errors.New("health cancellation observation changed")
		}
		newEpisode = fact.Automatic && before
	}
	if newEpisode {
		return createNativeIssue(ctx, tx, scope, request, now, true)
	}
	return issue, nil
}

func healthReportOccurrence(ctx context.Context, tx *sql.Tx, target hubhealth.Scope, finding hubhealth.Finding, item tracker.NativeWorkItemID, resolved, reportedResolved sql.NullString, now time.Time, cancellations map[string]hubhealth.Cancellation) (tracker.NativeIssue, bool, error) {
	scope := healthIssueScope(target.Organization, target.Project)
	var reported tracker.NativeIssue
	if resolved.Valid {
		issue, _, err := readNativeIssue(ctx, tx, scope, string(item))
		if err != nil {
			return tracker.NativeIssue{}, false, err
		}
		if !issue.Terminal && resolved != reportedResolved {
			body := fmt.Sprintf("## Health finding resolved\n\nFinding: `%s`\nResolved: %s", finding.ID, resolved.String)
			if _, err := insertNativeComment(ctx, tx, scope, issue, body, nil, now); err != nil {
				return tracker.NativeIssue{}, false, err
			}
		}
		reported = issue
	} else {
		request, err := hubhealth.IssueRequest(finding)
		if err != nil {
			return tracker.NativeIssue{}, false, err
		}
		contract, err := nativeIssueContract(ctx, tx, scope)
		if err != nil {
			return tracker.NativeIssue{}, false, err
		}
		target := "Todo"
		if contract.Context != "" && !contract.Evaluate(nativeContractIssue(tracker.NativeIssue{Body: request.Body})).Satisfied() {
			target = "Backlog"
		}
		request.State = target
		project, err := validateNativeIssueDraft(ctx, tx, scope, request)
		if err != nil {
			return tracker.NativeIssue{}, false, err
		}
		request.State = ""
		for _, state := range project.States {
			if state.Name == target && !state.Terminal && !state.OperatorOnly && state.Dispatchable == (target == "Todo") {
				request.State = target
				break
			}
		}
		if request.State == "" {
			return tracker.NativeIssue{}, false, nil
		}
		issue, err := reportHealthIssue(ctx, tx, scope, request, finding.Subject, item, finding.OpenedAt, now, cancellations)
		if err != nil {
			return tracker.NativeIssue{}, false, err
		}
		reported = issue
	}
	return reported, true, nil
}

func healthResolveIssue(ctx context.Context, tx *sql.Tx, target hubhealth.Scope, item string, now time.Time) error {
	scope := healthIssueScope(target.Organization, target.Project)
	issue, _, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return err
	}
	origin, ok := issueorigin.Parse(issue.Body)
	if !ok || origin.Instance != "health_detector" || issue.Actor != *scope.sourceActor || issue.Provenance != nil {
		return nil
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(project.States, func(state tracker.NativeState) bool {
		return state.Name == "Cancelled" && state.Terminal && !state.OperatorOnly
	}) {
		return nil
	}
	if err := validateNativeWorkflowTransition(project.States, issue.State, "Cancelled", apiScopeWorker); err != nil {
		return nil
	}
	ids, err := nativePageIDs(ctx, tx, "SELECT DISTINCT finding_id FROM health_finding_issues WHERE work_item_id=? ORDER BY finding_id LIMIT ?", item, hubhealth.ReadLimit+1)
	if err != nil {
		return err
	}
	if len(ids) > hubhealth.ReadLimit {
		return hubhealth.ErrReadLimit
	}
	from := issue.State
	issue.State = "Cancelled"
	data := tracker.CollaborationData{FromState: from, ToState: issue.State, Reason: "worker_progress", ReasonDetail: "Health findings resolved for the resolution window: " + strings.Join(ids, ", ")}
	if _, err := persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", data, now); err != nil {
		return err
	}
	return nil
}

func readHealthProductivityAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, attempts []hubhealth.RateAttempt) error {
	return newHealthSnapshotReader().ReadProductivityAttempts(ctx, q, hubhealth.Scope{Organization: scope.organization, Project: scope.project}, attempts)
}

func (s *Service) convertHealthProjections(ctx context.Context) error {
	return healthConversionStore(s).ConvertProjections(ctx, storageConversionLimit(ctx), storageProjectionStart(ctx, 0, 5))
}

func readHealthFindingLinks(ctx context.Context, q nativeQueryer, scope nativeScope, id string) ([]hubhealth.FindingIssue, error) {
	return hubhealth.ReadFindingLinks(ctx, q, healthFindingsScope(scope), id)
}

func readHealthLandings(ctx context.Context, q nativeQueryer, scope nativeScope, window operatortool.AnalyticsWindow) ([]nativeAnalyticsLanding, error) {
	return newHealthSnapshotReader().ReadLandings(ctx, q, hubhealth.Scope{Organization: scope.organization, Project: scope.project}, window)
}

func readHealthRateAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, window operatortool.AnalyticsWindow) ([]hubhealth.RateAttempt, error) {
	return newHealthSnapshotReader().ReadRateAttempts(ctx, q, hubhealth.Scope{Organization: scope.organization, Project: scope.project}, window)
}

func readLegacyHealthRateAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, window operatortool.AnalyticsWindow) ([]hubhealth.RateAttempt, error) {
	return newHealthSnapshotReader().ReadLegacyHealthRateAttempts(ctx, q, hubhealth.Scope{Organization: scope.organization, Project: scope.project}, window)
}

func (s *Service) getHealthFindings(c echo.Context) error {
	limit := 100
	if raw := c.QueryParam("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return s.nativeAPIError(c, nativePublicInvalid("limit must be between 1 and 100"))
		}
		limit = value
	}
	page, err := s.readHealthFindings(c.Request().Context(), nativeRequestScope(c), c.QueryParam("state"), c.QueryParam("since"), c.QueryParam("cursor"), limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readHealthFindings(ctx context.Context, scope nativeScope, state, since, cursor string, limit int) (hubhealth.FindingsPage, error) {
	return healthFindingsStore(s, scope).ReadFindings(ctx, healthFindingsScope(scope), state, since, cursor, limit)
}

func readHealthScheduled(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time) (hubhealth.ScheduledValidation, error) {
	return newHealthSnapshotReader().ReadScheduled(ctx, q, hubhealth.Scope{Organization: scope.organization, Project: scope.project}, now)
}

func readHealthSnapshot(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time) (hubhealth.Snapshot, error) {
	return newHealthSnapshotReader().ReadSnapshot(ctx, q, organization, now)
}

func readHealthSnapshotWithContracts(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time, contracts *hubhealth.ContractCache) (hubhealth.Snapshot, error) {
	return newHealthSnapshotReader().ReadSnapshotWithContracts(ctx, q, organization, now, contracts)
}

func readHealthCandidates(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time) ([]tracker.WorkItemID, error) {
	query := claimCandidateQuery{NativeScope: &scope, AvailableAt: now, PageSize: hubhealth.ReadLimit + 1}
	var candidates []tracker.WorkItemID
	for len(candidates) <= hubhealth.ReadLimit {
		query.Limit = hubhealth.ReadLimit + 1 - len(candidates)
		page, err := nativeCandidateIDs(ctx, q, query, nil, nil, nil, nil, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, page...)
		if len(page) < query.Limit {
			return candidates, nil
		}
		query.After = page[len(page)-1]
	}
	return nil, hubhealth.ErrReadLimit
}

func readHealthRunners(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time) ([]hubhealth.RunnerState, error) {
	return newHealthSnapshotReader().ReadRunners(ctx, q, organization, now)
}

func readHealthTimeEvents(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time) ([]hubhealth.TimeEvent, error) {
	return newHealthSnapshotReader().ReadTimeEvents(ctx, q, organization, now)
}

func readHealthEvents(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time, snapshot *hubhealth.Snapshot) error {
	return newHealthSnapshotReader().ReadEvents(ctx, q, organization, now, snapshot)
}

func readHealthCurrentWaits(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time, snapshot *hubhealth.Snapshot) error {
	return newHealthSnapshotReader().ReadCurrentWaits(ctx, q, organization, now, snapshot)
}

func readHealthDependencyHolds(ctx context.Context, q nativeQueryer, scope nativeScope) (int, int, []string, time.Time, error) {
	return newHealthSnapshotReader().ReadDependencyHolds(ctx, q, hubhealth.Scope{Organization: scope.organization, Project: scope.project})
}

func healthNativeScope(scope hubhealth.Scope) nativeScope {
	return nativeScope{organization: scope.Organization, project: scope.Project, credential: apiCredential{Scope: apiScopeAdmin}}
}

func healthContractLoad(ctx context.Context, q hubsql.Queryer, scope hubhealth.Scope, item string) (hubhealth.ContractObservation, error) {
	s := healthNativeScope(scope)
	issue, _, err := readNativeIssue(ctx, q, s, item)
	if err != nil {
		return hubhealth.ContractObservation{}, err
	}
	evaluation, err := evaluateNativeIssueContract(ctx, q, s, issue)
	if err != nil {
		return hubhealth.ContractObservation{}, err
	}
	return hubhealth.ContractObservation{Issue: issue, ReasonCode: evaluation.ReasonCode, Reason: evaluation.Reason, Cacheable: issue.LinkedSource == nil && (issue.IssueContract == nil || issue.IssueContract.ReleaseDelivery == nil)}, nil
}

func newHealthSnapshotReader() *hubhealth.SnapshotReader {
	return hubhealth.NewSnapshotReader(hubhealth.ReaderHooks{
		Candidates: func(ctx context.Context, q hubsql.Queryer, scope hubhealth.Scope, now time.Time) ([]tracker.WorkItemID, error) {
			return readHealthCandidates(ctx, q, healthNativeScope(scope), now)
		},
		Admissions: func(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID, runner string, machine tracker.MachineID) (map[string]tracker.NativeAdmissionObservation, error) {
			return readHealthAdmissionFacts(ctx, q, organization, runner, machine)
		},
		Repository: func(ctx context.Context, q hubsql.Queryer, scope string) (string, error) {
			return policyRepository(ctx, q, scope)
		}, DecodeStorageJSON: decodeStorageJSON, DecodeChangeJSON: decodeChangeColumnsJSON, ApplyAttemptData: applyHealthAttemptData, ColumnTime: changeColumnTime, LegacyReadLimit: changeLegacyReadLimit,
		ResidenceHistory: func(ctx context.Context, q hubsql.Queryer, scope hubhealth.Scope, window operatortool.AnalyticsWindow) (map[string]tracker.NativeState, []hubanalytics.ResidenceIssue, bool, error) {
			return readAnalyticsResidenceHistory(withAnalyticsLiveIssues(ctx), q, healthNativeScope(scope), window)
		}, Contract: healthContractLoad})
}

func healthStoreFor(writer, reader *sql.DB) *hubhealth.Store {
	return hubhealth.NewStore(writer, reader, healthBaseHooks())
}

func healthBaseHooks() hubhealth.Hooks {
	return hubhealth.Hooks{ReportOccurrence: healthReportOccurrence, ResolveIssue: healthResolveIssue, NewID: newNativeID, SyncProjects: syncHealthFindingProjects, AppendDashboard: appendDashboardHealthFinding, EnqueueOpened: enqueueSlackOpened, EnqueueResolved: enqueueSlackResolved}
}

func healthFindingsScope(scope nativeScope) hubhealth.Scope {
	return hubhealth.Scope{Organization: scope.organization, Project: scope.project, Reporter: scope.credential.HostedRole == "reporter", Principal: scope.actor().PrincipalID}
}

func healthFindingsStore(s *Service, scope nativeScope) *hubhealth.Store {
	h := healthBaseHooks()
	h.ReadTimeout = hubReadTimeout
	h.Invalid = nativePublicInvalid
	h.Watermark = func(ctx context.Context, q hubsql.Queryer) (int64, int64, error) {
		return readDashboardWatermark(ctx, q, scope)
	}
	h.ReadProject = func(ctx context.Context, q hubsql.Queryer) error {
		_, err := readNativeProject(ctx, q, scope)
		return err
	}
	h.PrepareQuery = func(ctx context.Context, tx *sql.Tx) (hubsql.Queryer, error) {
		return prepareNativePageQuery(ctx, tx, scope, "", "native_issue_actors")
	}
	h.Visibility = func() (string, []any) { return scope.issueVisibilitySQL("i") }
	return hubhealth.NewStore(s.database.db, s.database.reader, h)
}

func healthEvaluationStore(s *Service) *hubhealth.Store {
	h := healthBaseHooks()
	h.AfterCommit = s.afterHealthEvaluation
	h.SendEmails = s.sendHealthEmails
	return hubhealth.NewStore(s.database.db, s.database.reader, h)
}

func healthConversionStore(s *Service) *hubhealth.Store {
	h := healthBaseHooks()
	h.ChargeConversion = chargeStorageConversion
	h.ProjectionPage = func(ctx context.Context, key, query, columns string, limit int) ([]any, error) {
		positions, err := s.storageProjectionPage(ctx, key, query, columns, limit)
		if err != nil {
			return nil, err
		}
		ids := make([]any, len(positions))
		for i, p := range positions {
			ids[i] = p.id
		}
		return ids, nil
	}
	return hubhealth.NewStore(s.database.db, s.database.reader, h)
}
