package billingstore

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrInvalidAmount = errors.New("billing amount must be nonnegative")
var ErrInvalidUsage = errors.New("billing usage must have a positive amount and a valid window")
var ErrUsageReceiptConflict = errors.New("billing usage receipt conflicts with a previous request")
var ErrInvalidCollaborationBytes = errors.New("collaboration bytes adjustment is invalid")
var ErrInvalidBudgetFact = errors.New("monthly budget record is invalid")

const MaxUsageWindow = int64(1 << 50)

type PlanReference struct {
	ID      string
	Version int64
}

type Plan struct {
	OrganizationID tracker.OrganizationID
	PlanReference
	Name            string
	MonthlyUSDCents *int64
	RuntimeID       string
	Allowances      map[string]int64
	Features        []string
}

type Assignment struct {
	OrganizationID tracker.OrganizationID
	Base           PlanReference
	Subscription   *PlanReference
	ExpiresAt      *time.Time
	Revision       int64
}

type Grant struct {
	OrganizationID tracker.OrganizationID
	ID             string
	Plan           PlanReference
	Scope          []string
	StartsAt       time.Time
	ExpiresAt      *time.Time
	RevokedAt      *time.Time
}

type Checkout struct {
	Key       string
	PriceID   string
	ExpiresAt time.Time
	Session   billing.Session
}

type Account struct {
	OrganizationID tracker.OrganizationID
	Mode           string
	Binding        billing.Binding
	Snapshot       billing.Snapshot
	Status         string
	Plan           PlanReference
	PaidThrough    time.Time
	AccessUntil    time.Time
	GraceUntil     time.Time
	ReconciledAt   *time.Time
	Checkout       Checkout
}

type Price struct {
	OrganizationID tracker.OrganizationID
	PriceID        string
	Plan           PlanReference
}

type CreditPack struct {
	OrganizationID tracker.OrganizationID
	PriceID        string
	Mode           string
	USDCents       int64
}

type CustomerIntent struct {
	OrganizationID tracker.OrganizationID
	AccountID      string
	Mode           string
	IdempotencyKey string
	State          string
	CustomerID     string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Event struct {
	OrganizationID  tracker.OrganizationID
	Sequence        int64
	ID              string
	Type            string
	ReceivedAt      time.Time
	ProcessedAt     *time.Time
	CreditProcessed bool
}

type AuditReference struct {
	BindingID       string
	CustomerOwned   bool
	Key             string
	SHA256          string
	CompressedBytes int64
	ContentBytes    int64
	Codec           string
	FormatVersion   int
	Kind            string
	Retention       string
}

type PlanAudit struct {
	OrganizationID tracker.OrganizationID
	ID             int64
	CommandID      string
	RequestHash    string
	ActorID        string
	Action         string
	RecordedAt     time.Time
	Reference      AuditReference
}

type Audit struct {
	OrganizationID tracker.OrganizationID
	ID             int64
	ActorID        string
	Action         string
	RecordedAt     time.Time
	Reference      AuditReference
}

type RetiredAccount struct {
	OrganizationID tracker.OrganizationID
	AccountID      string
	CustomerID     string
	Mode           string
	RetiredAt      time.Time
	Reference      AuditReference
}

func AuditReferenceFromArchive(ref attachment.ArchiveReference) AuditReference {
	return AuditReference{BindingID: ref.BindingID, CustomerOwned: ref.CustomerOwned, Key: ref.Key, SHA256: ref.SHA256, CompressedBytes: ref.CompressedSize, ContentBytes: ref.UncompressedSize, Codec: ref.Codec, FormatVersion: ref.SchemaVersion, Kind: ref.Kind, Retention: ref.Retention}
}

func (ref AuditReference) ArchiveReference() attachment.ArchiveReference {
	return attachment.ArchiveReference{BindingID: ref.BindingID, CustomerOwned: ref.CustomerOwned, Key: ref.Key, SHA256: ref.SHA256, CompressedSize: ref.CompressedBytes, UncompressedSize: ref.ContentBytes, Codec: ref.Codec, SchemaVersion: ref.FormatVersion, Kind: ref.Kind, Retention: ref.Retention}
}

type CreditAccount struct {
	OrganizationID tracker.OrganizationID
	Mode           string
	BalanceMicros  int64
	AutoEnabled    bool
	ThresholdCents int64
	PriceID        string
	PaymentMethod  string
	Failure        string
}

type CreditPeriod struct {
	OrganizationID  tracker.OrganizationID
	Mode            string
	StartsAt        time.Time
	EndsAt          time.Time
	AllowanceMicros int64
	UsedMicros      int64
}

type CreditPurchase struct {
	OrganizationID tracker.OrganizationID
	Key            string
	Mode           string
	PriceID        string
	USDCents       int64
	Automatic      bool
	PaymentMethod  string
	State          string
	PaymentID      string
	Session        billing.Session
	CreatedAt      time.Time
}

type CreditTransaction struct {
	OrganizationID       tracker.OrganizationID
	ID                   int64
	Mode                 string
	Source               string
	AmountMicros         int64
	IncludedAmountMicros int64
	Kind                 string
	Actor                string
	Reason               string
	ReasonReference      AuditReference
	RecordedAt           time.Time
}

type CreditExportEntry struct {
	ID                   int64
	RecordedAt           time.Time
	Kind                 string
	TurnID               string
	Tokens               *int64
	AmountMicros         int64
	IncludedAmountMicros int64
}

type CreditHistoryEntry struct {
	ID                   int64
	AmountMicros         int64
	IncludedAmountMicros int64
	Kind                 string
	Actor                string
	Reason               string
	ReasonReference      AuditReference
	RecordedAt           time.Time
	Turns                int64
}

type MonthlyPolicy struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Revision       tracker.Revision
	Policy         budget.MonthlyPolicy
	Actor          string
	UpdatedAt      time.Time
}

type Usage struct {
	OrganizationID tracker.OrganizationID
	WindowStart    time.Time
	Metric         string
	Amount         int64
}

type UsageReceipt struct {
	OrganizationID tracker.OrganizationID
	ID             string
	WindowStart    time.Time
	Metric         string
	Amount         int64
}

type UsageResult struct {
	Applied bool
	Allowed bool
	Used    int64
}

type MonthlyAdmission struct {
	OrganizationID tracker.OrganizationID
	IssueID        tracker.WorkItemID
	AdmittedAt     time.Time
}

type MonthlyLease struct {
	OrganizationID       tracker.OrganizationID
	LeaseID              tracker.LeaseID
	LunaAPI              bool
	RunnerAPI            bool
	SpriteInfrastructure bool
}

type MonthlyBudgetInput struct {
	OrganizationPolicy MonthlyPolicy
	ProjectPolicy      MonthlyPolicy
	OrganizationSpend  budget.MonthlySpend
	ProjectSpend       budget.MonthlySpend
	Exposure           budget.CostExposure
	Admitted           bool
}

func EvaluateMonthlyBudget(input MonthlyBudgetInput) (budget.MonthlyDecision, error) {
	return budget.CheckMonthly(
		budget.MonthlyConstraint{Scope: "organization", Policy: input.OrganizationPolicy.Policy, Spend: input.OrganizationSpend},
		budget.MonthlyConstraint{Scope: "project", Policy: input.ProjectPolicy.Policy, Spend: input.ProjectSpend},
		input.Exposure,
		input.Admitted,
	)
}

type Reader interface {
	RuntimeReader
	ConversationUsageBuckets(context.Context, time.Time, time.Time, []string, bool) ([]ConversationUsageBucket, error)
	ConversationUsageSummary(context.Context, time.Time, time.Time, []string) (ConversationUsageSummary, error)
	ConversationPrice(context.Context, string, string, time.Time) (ConversationPrice, error)
	ConversationUsage(context.Context, string) (ConversationUsage, error)
	BillingCostRevision(context.Context, CostSource) (CostRevision, error)
	BillingMonthlySpend(context.Context, tracker.ProjectID, time.Time) (budget.MonthlySpend, error)
	BillingPlan(context.Context, PlanReference) (Plan, error)
	MissingConfiguredBillingPlan(context.Context, []PlanReference, time.Time) (PlanReference, error)
	BillingPlans(context.Context, PlanReference, int) ([]Plan, error)
	BillingAssignment(context.Context) (Assignment, error)
	BillingGrants(context.Context, string, int) ([]Grant, error)
	BillingGrant(context.Context, string) (Grant, error)
	BillingEffectiveGrants(context.Context, time.Time) ([]Grant, error)
	BillingFeatureEnabled(context.Context, string, time.Time, []string) (bool, error)
	BillingAccount(context.Context) (Account, error)
	BillingPrice(context.Context, string) (Price, error)
	CreditPack(context.Context, string, string) (CreditPack, error)
	BillingCustomerIntent(context.Context) (CustomerIntent, error)
	BillingEvents(context.Context, int64, int64, int) ([]Event, error)
	PendingBillingEvents(context.Context, int) ([]Event, error)
	UnprocessedBillingEvents(context.Context, int) ([]Event, error)
	BillingEvent(context.Context, string) (Event, error)
	BillingPlanAudit(context.Context, string) (PlanAudit, error)
	BillingPlanAudits(context.Context, int64, int) ([]PlanAudit, error)
	BillingAudits(context.Context, int64, int) ([]Audit, error)
	BillingCreditAccount(context.Context, string) (CreditAccount, error)
	BillingCreditPeriod(context.Context, string, time.Time, time.Time) (CreditPeriod, error)
	BillingCreditPurchase(context.Context, string) (CreditPurchase, error)
	BillingPendingCreditPurchase(context.Context, string, bool) (CreditPurchase, error)
	BillingCreditTransactions(context.Context, string, int64, int) ([]CreditTransaction, error)
	BillingRecentCreditTransactions(context.Context, string, int) ([]CreditTransaction, error)
	BillingCreditTransactionBySource(context.Context, string, string) (CreditTransaction, error)
	BillingCreditExport(context.Context, string, int64, int64, int64, time.Time, int64, int) (int64, []CreditExportEntry, error)
	BillingCreditHistory(context.Context, string, int64, int64, int64, int64, int64, int) (int64, int64, []CreditHistoryEntry, error)
	BillingCreditUsage(context.Context, string, time.Time, time.Time) (int64, error)
	BillingMonthlyPolicy(context.Context, tracker.ProjectID) (MonthlyPolicy, error)
	BillingMonthlyAdmission(context.Context, tracker.WorkItemID) (MonthlyAdmission, error)
	BillingMonthlyLease(context.Context, tracker.LeaseID) (MonthlyLease, error)
	BillingUsage(context.Context, time.Time, string) (int64, error)
	BillingUsages(context.Context, time.Time, int) ([]Usage, error)
	CollaborationBytes(context.Context) (int64, error)
}

type Writer interface {
	RuntimeWriter
	CreateConversationPrice(context.Context, ConversationPrice) error
	RecordConversationUsage(context.Context, ConversationUsage) (bool, error)
	Reader
	RecordBillingCost(context.Context, CostObservation) (bool, error)
	RecordBillingCosts(context.Context, []CostObservation) ([]bool, error)
	ImportBillingCostRevision(context.Context, CostObservation) (bool, error)
	CreateBillingPlan(context.Context, Plan) error
	ReplaceBillingPlan(context.Context, Plan) error
	SetBillingPlanRuntime(context.Context, PlanReference, string) error
	SetBillingAssignment(context.Context, Assignment, int64) error
	CreateBillingGrant(context.Context, Grant) error
	RevokeBillingGrant(context.Context, string, time.Time) error
	SetBillingAccount(context.Context, Account) error
	CreateBillingAccountIfAbsent(context.Context, Account) (bool, error)
	DeleteBillingAccount(context.Context) error
	DeleteBillingAccountIfMatches(context.Context, string, string, string) error
	CreateBillingPrice(context.Context, Price) error
	CreateCreditPack(context.Context, CreditPack) error
	CreateBillingCustomerIntent(context.Context, CustomerIntent) error
	SetBillingCustomerIntent(context.Context, CustomerIntent) error
	DeleteBillingCustomerIntent(context.Context, string) error
	CreateBillingEvent(context.Context, Event) (bool, error)
	SetBillingEventProcessed(context.Context, string, time.Time) error
	SetBillingEventCreditProcessed(context.Context, string) error
	CreateBillingPlanAudit(context.Context, PlanAudit) (bool, error)
	CreateBillingAudit(context.Context, Audit) error
	CreateRetiredBillingAccount(context.Context, RetiredAccount) error
	CreateBillingCreditAccount(context.Context, CreditAccount) error
	SetBillingCreditFunding(context.Context, CreditAccount) error
	CreateBillingCreditPurchase(context.Context, CreditPurchase) error
	SetBillingCreditPurchaseSession(context.Context, string, billing.Session) (billing.Session, error)
	MarkBillingCreditPurchasePaid(context.Context, string, string) error
	MarkBillingCreditPurchaseFailed(context.Context, string) (bool, error)
	PostBillingCredit(context.Context, CreditTransaction) (bool, error)
	PostBillingUsageCredit(context.Context, CreditPeriod, CreditTransaction, int64) (bool, error)
	SpendBillingIncludedCredit(context.Context, CreditPeriod, int64) (int64, error)
	SetBillingMonthlyPolicy(context.Context, MonthlyPolicy, tracker.Revision) error
	ImportBillingMonthlyPolicy(context.Context, MonthlyPolicy) error
	ApplyBillingUsage(context.Context, UsageReceipt, int64) (UsageResult, error)
	AdjustCollaborationBytes(context.Context, int64, int64) (UsageResult, error)
	PruneRequestBillingUsage(context.Context, time.Time, int) error
	RecordBillingMonthlyAdmission(context.Context, MonthlyAdmission) error
	RecordBillingMonthlyLease(context.Context, MonthlyLease) error
}
