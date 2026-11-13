package hubserver

import (
	"context"
	"database/sql"
	"net/url"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reports"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/usagereport"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const maxAnalyticsPopulation = reports.MaxAnalyticsPopulation

type nativeAnalyticsBucket = reports.NativeAnalyticsBucket
type nativeAnalyticsLanding = reports.NativeAnalyticsLanding
type nativeAnalyticsPhase = reports.NativeAnalyticsPhase
type nativeAnalyticsAttempt = reports.NativeAnalyticsAttempt
type nativeAnalyticsSkip = reports.NativeAnalyticsSkip
type nativeAnalyticsProject = reports.NativeAnalyticsProject
type nativeAnalyticsReport = reports.NativeAnalyticsReport

func nativeAnalyticsBucketIndex(p0 time.Time, p1 operatortool.AnalyticsWindow) int {
	return reports.NativeAnalyticsBucketIndex(p0, p1)
}

type analyticsCohortKey = reports.AnalyticsCohortKey
type analyticsCohortSelection = reports.AnalyticsCohortSelection

func analyticsCohortAllows(p0 context.Context, p1 tracker.NativeWorkItemID) bool {
	return reports.AnalyticsCohortAllows(p0, p1)
}
func bindAnalyticsCohort(p0 url.Values, p1 context.Context) {
	reports.BindAnalyticsCohort(p0, p1)
}
func analyticsCohortRunners(p0 context.Context) []string {
	return reports.AnalyticsCohortRunners(p0)
}
func analyticsCohortRunnerAllows(p0 context.Context, p1 string) bool {
	return reports.AnalyticsCohortRunnerAllows(p0, p1)
}
func summarizePipelineTimings(p0 []gate.PipelineTiming, p1 operatortool.AnalyticsWindow) reports.PipelineReport {
	return reports.SummarizePipelineTimings(p0, p1)
}
func uniquePipelineTimings(p0 []gate.PipelineTiming, p1 operatortool.AnalyticsWindow) []gate.PipelineTiming {
	return reports.UniquePipelineTimings(p0, p1)
}
func pipelineTimingPage(p0 []gate.PipelineTiming, p1 int, p2 int) operatortool.ReadPage[gate.PipelineTiming] {
	return reports.PipelineTimingPage(p0, p1, p2)
}

type analyticsQuality = reports.AnalyticsQuality
type qualityHistoryEvent = reports.QualityHistoryEvent
type qualityDiscussion = reports.QualityDiscussion
type analyticsQualityInputs = reports.AnalyticsQualityInputs

func projectAnalyticsQualityInputs(p0 reports.AnalyticsQualityInputs, p1 operatortool.AnalyticsRequest, p2 operatortool.AnalyticsWindow) (reports.AnalyticsQuality, error) {
	r0, r1 := reports.ProjectAnalyticsQualityInputs(p0, p1, p2)
	return r0, hubapi.FromFeatureError(r1)
}
func summarizeAnalyticsQuality(p0 []reports.NativeAnalyticsLanding, p1 []reports.QualityHistoryEvent, p2 []reports.QualityDiscussion, p3 operatortool.AnalyticsWindow) reports.AnalyticsQuality {
	return reports.SummarizeAnalyticsQuality(p0, p1, p2, p3)
}
func qualityCounts(p0 []reports.NativeAnalyticsLanding, p1 []reports.QualityHistoryEvent, p2 []reports.AnalyticsQualityEscape, p3 time.Time, p4 time.Time, p5 time.Time) reports.AnalyticsQualityCounts {
	return reports.QualityCounts(p0, p1, p2, p3, p4, p5)
}

type analyticsLandingPopulation = reports.AnalyticsLandingPopulation
type nativeAnalyticsAttemptPopulation = reports.NativeAnalyticsAttemptPopulation

func postgresReportOpenWork(p0 []reportstore.OpenFact, p1 time.Time, p2 []string) (reports.ReportsOpenWork, []string) {
	return reports.PostgresReportOpenWork(p0, p1, p2)
}
func summarizePostgresHostedReport(p0 []reportstore.OverviewFact, p1 []reportstore.AttemptFact, p2 map[reportstore.CostKey]float64, p3 bool, p4 bool, p5 bool, p6 []string, p7 operatortool.AnalyticsWindow, p8 operatortool.AnalyticsWindow, p9 bool) reports.HostedReportsReport {
	return reports.SummarizePostgresHostedReport(p0, p1, p2, p3, p4, p5, p6, p7, p8, p9)
}
func markPostgresReportCoverage(p0 *reports.HostedReportsReport, p1 []reportstore.ProjectHistory, p2 []reportstore.AttemptFact) {
	reports.MarkPostgresReportCoverage(p0, p1, p2)
}

type analyticsResidenceIssue = reports.AnalyticsResidenceIssue
type analyticsResidenceEvent = reports.AnalyticsResidenceEvent
type analyticsResidenceTimeline = reports.AnalyticsResidenceTimeline

func projectAnalyticsResidencePopulation(p0 map[string]tracker.NativeState, p1 []reports.AnalyticsResidenceIssue, p2 bool, p3 operatortool.AnalyticsRequest, p4 *reports.NativeAnalyticsProject) error {
	r0 := reports.ProjectAnalyticsResidencePopulation(p0, p1, p2, p3, p4)
	return hubapi.FromFeatureError(r0)
}

type analyticsLiveIssuesKey = reports.AnalyticsLiveIssuesKey

func withAnalyticsLiveIssues(p0 context.Context) context.Context {
	return reports.WithAnalyticsLiveIssues(p0)
}
func buildAnalyticsResidenceTimeline(p0 reports.AnalyticsResidenceIssue, p1 map[string]tracker.NativeState, p2 time.Time) reports.AnalyticsResidenceTimeline {
	return reports.BuildAnalyticsResidenceTimeline(p0, p1, p2)
}

type analyticsAttemptLanding = reports.AnalyticsAttemptLanding

func analyticsFailureTime(p0 *reports.AnalyticsAttemptLanding, p1 string, p2 string) sql.NullString {
	return reports.AnalyticsFailureTime(p0, p1, p2)
}

type nativeAnalyticsCapacityWait = reports.NativeAnalyticsCapacityWait

func nativeAnalyticsCapacityInterval(p0 *tracker.NativeSchedulerDecision, p1 *tracker.NativeSchedulerDecision, p2 time.Time, p3 time.Time) bool {
	return reports.NativeAnalyticsCapacityInterval(p0, p1, p2, p3)
}

type nativeAnalyticsSchedulerPopulation = reports.NativeAnalyticsSchedulerPopulation

func applyPostgresReportHistory(p0 *reports.HostedReportsReport, p1 []reportstore.ProjectHistory, p2 []reportstore.OverviewFact, p3 operatortool.AnalyticsWindow, p4 operatortool.AnalyticsWindow) {
	reports.ApplyPostgresReportHistory(p0, p1, p2, p3, p4)
}
func applyPostgresReportLandings(p0 *reports.HostedReportsReport, p1 []reportstore.LandingFact, p2 operatortool.AnalyticsWindow, p3 operatortool.AnalyticsWindow, p4 bool) {
	reports.ApplyPostgresReportLandings(p0, p1, p2, p3, p4)
}
func applyPostgresReportFailures(p0 *reports.HostedReportsReport, p1 []reportstore.AttemptFact, p2 operatortool.AnalyticsWindow, p3 operatortool.AnalyticsWindow, p4 bool) {
	reports.ApplyPostgresReportFailures(p0, p1, p2, p3, p4)
}

type hostedReportsReport = reports.HostedReportsReport

func reportFailureClasses(p0 []reports.NativeFailure) []reports.ReportClassTotal {
	return reports.ReportFailureClasses(p0)
}

type productivityCount = reports.ProductivityCount
type productivityBucket = reports.ProductivityBucket
type productivityActor = reports.ProductivityActor
type organizationProductivity = reports.OrganizationProductivity

func productivityBuckets(p0 operatortool.AnalyticsWindow, p1 []string) []reports.ProductivityBucket {
	return reports.ProductivityBuckets(p0, p1)
}
func productivityBucketCount(p0 []reports.ProductivityBucket, p1 operatortool.AnalyticsWindow, p2 time.Time, p3 string, p4 bool) {
	reports.ProductivityBucketCount(p0, p1, p2, p3, p4)
}
func productivityActorCount(p0 map[string]*reports.ProductivityActor, p1 string, p2 string, p3 string, p4 bool) {
	reports.ProductivityActorCount(p0, p1, p2, p3, p4)
}
func productivityActors(p0 map[string]*reports.ProductivityActor) []reports.ProductivityActor {
	return reports.ProductivityActors(p0)
}
func filterProductivityProjects(p0 []string, p1 []string) []string {
	return reports.FilterProductivityProjects(p0, p1)
}
func withoutPriorPeriod(p0 reports.HostedReportsReport) reports.HostedReportsReport {
	return reports.WithoutPriorPeriod(p0)
}

const maxFailureExampleLength = reports.MaxFailureExampleLength

type nativeFailureSignature = reports.NativeFailureSignature
type nativeFailure = reports.NativeFailure

func failureFirstLine(p0 string) string {
	return reports.FailureFirstLine(p0)
}
func normalizeFailureSignature(p0 string) string {
	return reports.NormalizeFailureSignature(p0)
}
func failureFromAttempt(p0 tracker.NativeRunData, p1 string) (string, string) {
	return reports.FailureFromAttempt(p0, p1)
}
func groupNativeFailures(p0 []reports.NativeFailure) []reports.NativeFailureSignature {
	return reports.GroupNativeFailures(p0)
}
func latestNativeFailureSignature(p0 []reports.NativeFailure, p1 bool) *explain.FailureSignature {
	return reports.LatestNativeFailureSignature(p0, p1)
}
func nativeFailureSignaturePage(p0 []reports.NativeFailureSignature, p1 int, p2 int) operatortool.ReadPage[reports.NativeFailureSignature] {
	return reports.NativeFailureSignaturePage(p0, p1, p2)
}
func diagnosticNativeFailures(p0 diagnosticstore.Snapshot, p1 map[string]tracker.NativeRunData, p2 operatortool.AnalyticsWindow) []reports.NativeFailure {
	return reports.DiagnosticNativeFailures(p0, p1, p2)
}

type postgresReportUsage = reports.PostgresReportUsage

func readPostgresReportUsage(p0 context.Context, p1 tenantstore.Reader, p2 reportstore.OverviewQuery, p3 []reportstore.OverviewFact, p4 operatortool.AnalyticsWindow, p5 operatortool.AnalyticsWindow) (reports.PostgresReportUsage, error) {
	r0, r1 := reports.ReadPostgresReportUsage(p0, p1, p2, p3, p4, p5)
	return r0, hubapi.FromFeatureError(r1)
}
func postgresReportUsageRows(p0 []reportstore.UsageFact) (map[string][]usagereport.Row, error) {
	r0, r1 := reports.PostgresReportUsageRows(p0)
	return r0, hubapi.FromFeatureError(r1)
}
func applyPostgresReportUsage(p0 *reports.HostedReportsReport, p1 []reportstore.OverviewFact, p2 []reportstore.AttemptFact, p3 []reportstore.UsageFact, p4 operatortool.AnalyticsWindow, p5 operatortool.AnalyticsWindow, p6 bool) error {
	r0 := reports.ApplyPostgresReportUsage(p0, p1, p2, p3, p4, p5, p6)
	return hubapi.FromFeatureError(r0)
}
func readPostgresReportCompletionCosts(p0 context.Context, p1 tenantstore.Reader, p2 reportstore.OverviewQuery, p3 []reportstore.OverviewFact, p4 operatortool.AnalyticsWindow, p5 operatortool.AnalyticsWindow, p6 bool) (map[reportstore.CompletionCostItem]reportstore.CompletionCostFact, error) {
	r0, r1 := reports.ReadPostgresReportCompletionCosts(p0, p1, p2, p3, p4, p5, p6)
	return r0, hubapi.FromFeatureError(r1)
}
func applyPostgresReportCompletionCosts(p0 *reports.HostedReportsReport, p1 []reportstore.OverviewFact, p2 map[reportstore.CompletionCostItem]reportstore.CompletionCostFact, p3 operatortool.AnalyticsWindow, p4 operatortool.AnalyticsWindow, p5 bool) {
	reports.ApplyPostgresReportCompletionCosts(p0, p1, p2, p3, p4, p5)
}

type analyticsPagingKey = reports.AnalyticsPagingKey

func analyticsPagingContext(p0 context.Context, p1 string) (context.Context, *reports.AnalyticsPaging, error) {
	r0, r1, r2 := reports.AnalyticsPagingContext(p0, p1)
	return r0, r1, hubapi.FromFeatureError(r2)
}
func analyticsPagingFromContext(p0 context.Context) *reports.AnalyticsPaging {
	return reports.AnalyticsPagingFromContext(p0)
}

type analyticsResidenceCursor = reports.AnalyticsResidenceCursor
type analyticsFactCursor = reports.AnalyticsFactCursor
type diagnosticsCapacity = reports.DiagnosticsCapacity
type diagnosticsReport = reports.DiagnosticsReport
type diagnosticsRefusal = reports.DiagnosticsRefusal
type diagnosticsConfiguration = reports.DiagnosticsConfiguration

func reportsTrackingFingerprints(p0 []reports.NativeFailure) map[string]string {
	return reports.ReportsTrackingFingerprints(p0)
}
func projectNativeAnalyticsActivity(p0 *reports.NativeAnalyticsProject, p1 *reports.NativeAnalyticsAttempt, p2 string) {
	reports.ProjectNativeAnalyticsActivity(p0, p1, p2)
}
func nativeAnalyticsAttemptPage(p0 []reports.NativeAnalyticsAttempt, p1 int, p2 int) operatortool.ReadPage[reports.NativeAnalyticsAttempt] {
	return reports.NativeAnalyticsAttemptPage(p0, p1, p2)
}
func projectTenantDiagnostics(p0 diagnosticstore.Snapshot, p1 operatortool.AnalyticsWindow) reports.DiagnosticsReport {
	return reports.ProjectTenantDiagnostics(p0, p1)
}
func populateTenantAttemptDiagnostics(p0 *reports.DiagnosticsReport, p1 diagnosticstore.Snapshot, p2 map[string]tracker.NativeRunData, p3 operatortool.AnalyticsWindow) (int, int) {
	return reports.PopulateTenantAttemptDiagnostics(p0, p1, p2, p3)
}

type reportClassTotal = reports.ReportClassTotal

func classifyFailure(p0 string) reports.ReportClass {
	return reports.ClassifyFailure(p0)
}
func applyPostgresReportQuality(p0 *reports.HostedReportsReport, p1 []reportstore.ProjectHistory, p2 []reportstore.LandingFact, p3 []reportstore.EscapeFact, p4 []reportstore.OverviewFact, p5 operatortool.AnalyticsWindow, p6 operatortool.AnalyticsWindow) {
	reports.ApplyPostgresReportQuality(p0, p1, p2, p3, p4, p5, p6)
}

const reportsAllProjects = reports.ReportsAllProjects
const reportsMaxFilterValues = reports.ReportsMaxFilterValues
const reportsFacetLimit = reports.ReportsFacetLimit
const reportsUnavailableName = reports.ReportsUnavailableName

type reportsFilter = reports.ReportsFilter

func reportsQueryValues(p0 url.Values, p1 string) []string {
	return reports.ReportsQueryValues(p0, p1)
}
func parseReportsFilter(p0 url.Values) (reports.ReportsFilter, error) {
	r0, r1 := reports.ParseReportsFilter(p0)
	return r0, hubapi.FromFeatureError(r1)
}
func reportsWindow(p0 url.Values, p1 time.Time) (operatortool.AnalyticsWindow, error) {
	r0, r1 := reports.ReportsWindow(p0, p1)
	return r0, hubapi.FromFeatureError(r1)
}

type reportsScopeKey = reports.ReportsScopeKey

func reportsScopeFrom(p0 context.Context) (reports.ReportsScope, bool) {
	return reports.ReportsScopeFrom(p0)
}

type reportsFacet = reports.ReportsFacet
type reportsFacets = reports.ReportsFacets
