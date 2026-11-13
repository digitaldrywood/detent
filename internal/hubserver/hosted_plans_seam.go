package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d *database) plansService() *hostedplans.Service {
	return hostedplans.New(hostedplans.Deps{Repository: d.billingRepository, Details: d.billingAuditStore, Organization: d.hostedOrganization, Runtime: &d.hostedRuntime, Loads: &d.runtimeLoads, RuntimeBilling: d.runtimeBilling, HostedBilling: d.hostedBilling, Logger: d.logger, Now: d.now, CurrentTime: d.currentTime, Configured: func(mode string, multiplier float64, credits bool) {
		d.hostedBilling = true
		if credits {
			d.aiCreditMode = mode
			d.aiCreditCostMultiplier = multiplier
		} else {
			d.aiCreditMode = ""
		}
	}})
}

func hostedPlansConfig(cfg *HostedConfig) *hostedplans.Hosted {
	if cfg == nil {
		return nil
	}
	return &hostedplans.Hosted{Billing: cfg.Billing, Plans: cfg.Plans, PlanID: cfg.PlanID, StorageQuotaBytes: cfg.StorageQuotaBytes, EventQuota: cfg.EventQuota}
}

type hostedConsumptionRecord = hostedplans.ConsumptionRecord
type hostedBillingState = hostedplans.BillingState
type hostedPlanCommand = hostedplans.PlanCommand
type hostedEntitlementReport = hostedplans.EntitlementReport
type hostedLimitError = hostedplans.LimitError

func resolveHostedBilling(p0 *hostedplans.HostedBillingConfig, p1 hostedplans.BillingState, p2 billing.Snapshot, p3 time.Time) hostedplans.BillingState {
	return hostedplans.ResolveBilling(p0, p1, p2, p3)
}
func hostedCapacityPlan(p0 hostedplans.PlanReference) bool {
	return hostedplans.CapacityPlan(p0)
}
func normalizeHostedPlan(p0 hostedplans.HostedPlan) hostedplans.HostedPlan {
	return hostedplans.NormalizePlan(p0)
}
func hostedPlanPrice(p0 *int64) string {
	return hostedplans.PlanPrice(p0)
}
func hostedAllowanceNames() []string {
	return hostedplans.AllowanceNames()
}
func pilotHostedPlans() hostedplans.HostedPlansConfig {
	return hostedplans.PilotPlans()
}
func hostedTiers() []hostedplans.Tier {
	return hostedplans.Tiers()
}
func capacityHostedPlans() hostedplans.HostedPlansConfig {
	return hostedplans.CapacityPlans()
}
func defaultHostedPlans(p0 *HostedConfig) hostedplans.HostedPlansConfig {
	return hostedplans.DefaultPlans(hostedPlansConfig(p0))
}
func requireHostedFeatureInEntitlement(p0 hostedplans.HostedEntitlement, p1 string) error {
	r0 := hostedplans.RequireFeatureInEntitlement(p0, p1)
	return hubapi.FromFeatureError(r0)
}

var errHostedBillingUnbound = hostedplans.ErrBillingUnbound
var errHostedEntitlementsDisabled = hostedplans.ErrEntitlementsDisabled

const defaultAICreditCostMultiplier = hostedplans.DefaultAICreditCostMultiplier

func (d *database) readHostedConsumptionRecord(p0 context.Context, p1 tenantstore.Reader, p2 time.Time, p3 ...string) (hostedplans.ConsumptionRecord, error) {
	r0, r1 := d.plansService().ReadConsumptionRecord(p0, p1, p2, p3...)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) hostedConsumption(p0 context.Context, p1 hostedplans.Queryer, p2 time.Time, p3 ...string) (map[string]int64, error) {
	r0, r1 := d.plansService().Consumption(p0, p1, p2, p3...)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) recordHostedUsage(p0 context.Context, p1 *sql.Tx, p2 time.Time, p3 string, p4 int64) error {
	r0 := d.plansService().RecordUsage(p0, p1, p2, p3, p4)
	return hubapi.FromFeatureError(r0)
}
func (d *database) readHostedPlanRecord(p0 context.Context, p1 billingstore.Reader, p2 hostedplans.PlanReference) (hostedplans.HostedPlan, error) {
	r0, r1 := d.plansService().ReadPlanRecord(p0, p1, p2)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) readBillingTenant(p0 context.Context, p1 func(tenantstore.Reader) error) error {
	r0 := d.plansService().ReadBillingTenant(p0, p1)
	return hubapi.FromFeatureError(r0)
}
func (d *database) writeBillingTenant(p0 context.Context, p1 func(tenantstore.Writer) error) error {
	r0 := d.plansService().WriteBillingTenant(p0, p1)
	return hubapi.FromFeatureError(r0)
}
func (d *database) archiveBillingReason(p0 context.Context, p1 string) (billingstore.AuditReference, error) {
	r0, r1 := d.plansService().ArchiveBillingReason(p0, p1)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) readBillingReason(p0 context.Context, p1 billingstore.AuditReference) (string, error) {
	r0, r1 := d.plansService().ReadBillingReason(p0, p1)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) hostedEntitlementRecord(p0 context.Context, p1 billingstore.Reader, p2 time.Time) (hostedplans.HostedEntitlement, error) {
	r0, r1 := d.plansService().EntitlementRecord(p0, p1, p2)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) readHostedBilling(p0 context.Context) (hostedplans.BillingState, error) {
	r0, r1 := d.plansService().ReadBilling(p0)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) commitHostedBilling(p0 context.Context, p1 hostedplans.BillingState, p2 int64, p3 []string, p4 time.Time) error {
	r0 := d.plansService().CommitBilling(p0, p1, p2, p3, p4)
	return hubapi.FromFeatureError(r0)
}
func (d *database) refreshRuntime(p0 context.Context) error {
	r0 := d.plansService().RefreshRuntime(p0)
	return hubapi.FromFeatureError(r0)
}
func (d *database) applyRuntimeRecords(p0 context.Context, p1 hostedplans.HostedPlansConfig, p2 []hostedplans.HostedBillingPrice, p3 string, p4 string, p5 string, p6 bool) error {
	r0 := d.plansService().ApplyRuntimeRecords(p0, p1, p2, p3, p4, p5, p6)
	return hubapi.FromFeatureError(r0)
}
func (d *database) hostedBillingBinding(p0 context.Context, p1 *hostedplans.HostedBillingConfig) (billing.Binding, error) {
	r0, r1 := d.plansService().BillingBinding(p0, p1)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) configureHostedBilling(p0 context.Context, p1 *HostedConfig) error {
	r0 := d.plansService().ConfigureBilling(p0, hostedPlansConfig(p1))
	return hubapi.FromFeatureError(r0)
}
func (d *database) hostedEntitlementReport(p0 context.Context, p1 int64, p2 int) (hostedplans.EntitlementReport, error) {
	r0, r1 := d.plansService().Entitlements(p0, p1, p2)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) applyHostedPlanCommand(p0 context.Context, p1 string, p2 hostedplans.PlanCommand) error {
	r0 := d.plansService().ApplyPlanCommand(p0, p1, p2)
	return hubapi.FromFeatureError(r0)
}
func (d *database) hostedPlanUsage(p0 context.Context, p1 time.Time, p2 ...string) (hostedplans.HostedEntitlement, error) {
	r0, r1 := d.plansService().PlanUsage(p0, p1, p2...)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) readHostedPlanUsage(p0 context.Context, p1 hostedplans.Queryer, p2 time.Time, p3 ...string) (hostedplans.HostedEntitlement, error) {
	r0, r1 := d.plansService().ReadPlanUsage(p0, p1, p2, p3...)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) readHostedPlan(p0 context.Context, p1 hostedplans.Queryer, p2 hostedplans.PlanReference) (hostedplans.HostedPlan, error) {
	r0, r1 := d.plansService().ReadPlan(p0, p1, p2)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) hostedEntitlement(p0 context.Context, p1 hostedplans.Queryer, p2 time.Time) (hostedplans.HostedEntitlement, error) {
	r0, r1 := d.plansService().Entitlement(p0, p1, p2)
	return r0, hubapi.FromFeatureError(r1)
}
func (d *database) requireHostedFeatureRecord(p0 context.Context, p1 billingstore.Reader, p2 string, p3 time.Time) error {
	r0 := d.plansService().RequireFeatureRecord(p0, p1, p2, p3)
	return hubapi.FromFeatureError(r0)
}
func (d *database) configureHostedPlans(p0 context.Context, p1 *HostedConfig) error {
	r0 := d.plansService().ConfigurePlans(p0, hostedPlansConfig(p1))
	return hubapi.FromFeatureError(r0)
}
func (d *database) runtimeState() *hostedplans.RuntimeConfig {
	return d.plansService().RuntimeState()
}
func (d *database) plans() *hostedplans.HostedPlansConfig {
	return d.plansService().Plans()
}

type PlanReference = hostedplans.PlanReference
type HostedPlan = hostedplans.HostedPlan
type HostedPlansConfig = hostedplans.HostedPlansConfig
type HostedGrant = hostedplans.HostedGrant
type HostedEntitlement = hostedplans.HostedEntitlement
type HostedBillingPrice = hostedplans.HostedBillingPrice
type HostedCreditPack = hostedplans.HostedCreditPack
type HostedBillingConfig = hostedplans.HostedBillingConfig
type HostedRuntimeUpdate = hostedplans.HostedRuntimeUpdate
type HostedRuntimeSnapshots = hostedplans.HostedRuntimeSnapshots
type hostedRuntimeConfig = hostedplans.RuntimeConfig

var ErrBillingUsageUnavailable = hostedplans.ErrBillingUsageUnavailable

func NewHostedRuntimeSnapshots(p0 tenantstore.Repository, p1 attachment.DetailStore) (*hostedplans.HostedRuntimeSnapshots, error) {
	r0, r1 := hostedplans.NewHostedRuntimeSnapshots(p0, p1)
	return r0, hubapi.FromFeatureError(r1)
}
func CheckoutHostedPlans(p0 *hostedplans.HostedPlansConfig) *hostedplans.HostedPlansConfig {
	return hostedplans.CheckoutHostedPlans(p0)
}
func ValidateHostedRuntimeUpdate(p0 hostedplans.HostedRuntimeUpdate) error {
	r0 := hostedplans.ValidateHostedRuntimeUpdate(p0)
	return hubapi.FromFeatureError(r0)
}
func hostedSafeID(p0 string) bool {
	return hostedplans.SafeID(p0)
}
