-- name: BillingCreditAccount :one
SELECT * FROM ai_credit_accounts WHERE organization_id=$1 AND mode=$2;

-- name: LockBillingCreditAccount :one
SELECT * FROM ai_credit_accounts WHERE organization_id=$1 AND mode=$2 FOR UPDATE;

-- name: CreateBillingCreditAccount :exec
INSERT INTO ai_credit_accounts(organization_id,mode,balance_micros,auto_enabled,threshold_cents,price_id,payment_method,failure)
VALUES($1,$2,$3,$4,$5,$6,$7,$8);

-- name: SetBillingCreditFunding :execrows
UPDATE ai_credit_accounts SET auto_enabled=$3,threshold_cents=$4,price_id=$5,payment_method=$6,failure=$7 WHERE organization_id=$1 AND mode=$2;

-- name: BillingCreditTransactionBySource :one
SELECT t.organization_id,t.id,t.mode,t.source,t.amount_micros,t.included_amount_micros,t.kind,t.actor,t.recorded_at,
       r.binding_id,r.customer_owned,r.object_key,r.sha256,r.compressed_bytes,r.content_bytes,r.codec,r.format_version,r.kind AS reference_kind,r.retention
FROM ai_credit_transactions t LEFT JOIN ai_credit_reason_references r USING (organization_id,mode,source)
WHERE t.organization_id=$1 AND t.mode=$2 AND t.source=$3;

-- name: CreateBillingCreditTransaction :exec
INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,included_amount_micros,kind,actor,recorded_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8);

-- name: CreateBillingCreditReasonReference :exec
INSERT INTO ai_credit_reason_references(organization_id,mode,source,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13);

-- name: AddBillingCreditBalance :execrows
UPDATE ai_credit_accounts SET balance_micros=balance_micros+$3 WHERE organization_id=$1 AND mode=$2;

-- name: BillingCreditTransactions :many
SELECT t.organization_id,t.id,t.mode,t.source,t.amount_micros,t.included_amount_micros,t.kind,t.actor,t.recorded_at,
       r.binding_id,r.customer_owned,r.object_key,r.sha256,r.compressed_bytes,r.content_bytes,r.codec,r.format_version,r.kind AS reference_kind,r.retention
FROM ai_credit_transactions t LEFT JOIN ai_credit_reason_references r USING (organization_id,mode,source)
WHERE t.organization_id=$1 AND t.mode=$2 AND t.id>sqlc.arg(after_id) ORDER BY t.id LIMIT sqlc.arg(page_size);

-- name: BillingRecentCreditTransactions :many
SELECT t.organization_id,t.id,t.mode,t.source,t.amount_micros,t.included_amount_micros,t.kind,t.actor,t.recorded_at,
       r.binding_id,r.customer_owned,r.object_key,r.sha256,r.compressed_bytes,r.content_bytes,r.codec,r.format_version,r.kind AS reference_kind,r.retention
FROM ai_credit_transactions t LEFT JOIN ai_credit_reason_references r USING (organization_id,mode,source)
WHERE t.organization_id=$1 AND t.mode=$2 AND t.kind IN ('complimentary','purchase','auto_fund') ORDER BY t.recorded_at DESC,t.id DESC LIMIT $3;

-- name: BillingCreditPeriod :one
SELECT * FROM ai_credit_periods WHERE organization_id=$1 AND mode=$2 AND period_start=$3 AND period_end=$4;

-- name: EnsureBillingCreditPeriod :exec
INSERT INTO ai_credit_periods(organization_id,mode,period_start,period_end,allowance_micros,used_micros)
VALUES($1,$2,$3,$4,$5,0) ON CONFLICT(organization_id,mode,period_start,period_end) DO NOTHING;

-- name: LockBillingCreditPeriod :one
SELECT * FROM ai_credit_periods WHERE organization_id=$1 AND mode=$2 AND period_start=$3 AND period_end=$4 FOR UPDATE;

-- name: SpendBillingCreditPeriod :exec
UPDATE ai_credit_periods SET used_micros=used_micros+$5 WHERE organization_id=$1 AND mode=$2 AND period_start=$3 AND period_end=$4;

-- name: BillingCreditPurchase :one
SELECT * FROM ai_credit_purchases WHERE organization_id=$1 AND purchase_key=$2;

-- name: CreateBillingCreditPurchase :exec
INSERT INTO ai_credit_purchases(organization_id,purchase_key,mode,price_id,usd_cents,automatic,payment_method,state,payment_id,session_id,session_url,session_expires_at,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13);

-- name: SetBillingCreditPurchaseSession :execrows
UPDATE ai_credit_purchases SET session_id=$3,session_url=$4,session_expires_at=$5
WHERE organization_id=$1 AND purchase_key=$2 AND (session_id='' OR session_id=$3);

-- name: MarkBillingCreditPurchasePaid :execrows
UPDATE ai_credit_purchases SET state='paid',payment_id=$3
WHERE organization_id=$1 AND purchase_key=$2 AND (state IN ('pending','failed') OR state='paid' AND payment_id=$3);

-- name: MarkBillingCreditPurchaseFailed :execrows
UPDATE ai_credit_purchases SET state='failed'
WHERE organization_id=$1 AND purchase_key=$2 AND state='pending';

-- name: BillingUsage :one
SELECT amount FROM hosted_usage_windows WHERE organization_id=$1 AND window_start=$2 AND metric=$3;

-- name: BillingUsages :many
SELECT organization_id,window_start,metric,amount FROM hosted_usage_windows WHERE organization_id=$1 AND window_start=$2 ORDER BY metric LIMIT $3;

-- name: EnsureBillingUsageWindow :exec
INSERT INTO hosted_usage_windows(organization_id,window_start,metric,amount) VALUES($1,$2,$3,0)
ON CONFLICT(organization_id,window_start,metric) DO NOTHING;

-- name: LockBillingUsageWindow :one
SELECT amount FROM hosted_usage_windows WHERE organization_id=$1 AND window_start=$2 AND metric=$3 FOR UPDATE;

-- name: BillingUsageReceipt :one
SELECT receipt_id,window_start,metric,amount FROM hosted_usage_receipts WHERE organization_id=$1 AND receipt_id=$2;

-- name: CreateBillingUsageReceipt :execrows
INSERT INTO hosted_usage_receipts(organization_id,receipt_id,window_start,metric,amount) VALUES($1,$2,$3,$4,$5)
ON CONFLICT(organization_id,receipt_id) DO NOTHING;

-- name: BillingMonthlyAdmission :one
SELECT organization_id,issue_id,admitted_at FROM monthly_budget_admissions WHERE organization_id=$1 AND issue_id=$2;

-- name: RecordBillingMonthlyAdmission :execrows
INSERT INTO monthly_budget_admissions(organization_id,issue_id,admitted_at) VALUES($1,$2,$3)
ON CONFLICT(organization_id,issue_id) DO NOTHING;

-- name: BillingMonthlyLease :one
SELECT organization_id,lease_id,luna_api,runner_api,sprite_infrastructure FROM monthly_budget_leases WHERE organization_id=$1 AND lease_id=$2;

-- name: RecordBillingMonthlyLease :execrows
INSERT INTO monthly_budget_leases(organization_id,lease_id,luna_api,runner_api,sprite_infrastructure) VALUES($1,$2,$3,$4,$5)
ON CONFLICT(organization_id,lease_id) DO NOTHING;

-- name: AddBillingUsage :exec
INSERT INTO hosted_usage_windows(organization_id,window_start,metric,amount) VALUES($1,$2,$3,$4)
ON CONFLICT(organization_id,window_start,metric) DO UPDATE SET amount=hosted_usage_windows.amount+excluded.amount;

-- name: ChargeBillingUsageWindow :one
INSERT INTO hosted_usage_windows(organization_id,window_start,metric,amount)
SELECT sqlc.arg(organization_id),sqlc.arg(window_start),sqlc.arg(metric),sqlc.arg(amount)::bigint
WHERE sqlc.arg(amount)::bigint <= sqlc.arg(allowance)::bigint
ON CONFLICT(organization_id,window_start,metric) DO UPDATE SET amount=hosted_usage_windows.amount+excluded.amount
WHERE hosted_usage_windows.amount <= sqlc.arg(allowance)::bigint-excluded.amount
RETURNING amount;

-- name: BillingMonthlyAdmittedCandidate :one
WITH admitted AS MATERIALIZED (
 SELECT a.organization_id,a.issue_id FROM monthly_budget_admissions a
 JOIN issues cohort_item ON cohort_item.organization_id=a.organization_id AND cohort_item.id=a.issue_id
 WHERE a.organization_id=sqlc.arg(organization_id) AND cohort_item.project_id=sqlc.arg(project_id)
)
SELECT EXISTS (
 SELECT 1 FROM admitted a
 JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
 JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
 JOIN issue_claim_facts f ON f.organization_id=i.organization_id AND f.project_id=i.project_id AND f.work_item_id=i.native_id
 WHERE a.organization_id=sqlc.arg(organization_id)
 AND i.project_id=sqlc.arg(project_id)
 AND p.profile='native' AND p.deleted_at IS NULL AND i.archived=false
 AND w.dispatchable=true AND w.terminal=false
 AND EXISTS (SELECT 1 FROM queue_entries q WHERE q.organization_id=i.organization_id AND q.project_id=i.project_id AND q.work_item_id=i.native_id AND q.scope=i.project_id)
 AND NOT EXISTS (SELECT 1 FROM workspace_items wi WHERE wi.organization_id=i.organization_id AND wi.project_id=i.project_id AND wi.work_item_id=i.native_id)
 AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.organization_id=i.organization_id AND l.issue_id=i.id AND l.released_at IS NULL AND l.expires_at>sqlc.arg(available_at)::timestamptz)
 AND (NOT p.require_dependencies OR NOT EXISTS (
  SELECT 1 FROM issue_dependencies d
  JOIN issues blocker ON blocker.organization_id=d.organization_id AND blocker.project_id=d.blocker_project_id AND blocker.id=d.blocker_issue_id
  LEFT JOIN project_workflow_states blocker_state ON blocker_state.organization_id=blocker.organization_id AND blocker_state.project_id=blocker.project_id AND blocker_state.name=blocker.workflow_state_name
  WHERE d.organization_id=i.organization_id AND d.dependent_project_id=i.project_id AND d.dependent_issue_id=i.id
  AND (blocker_state.name IS NULL OR blocker_state.terminal=false)
 ))
);

-- name: BillingMonthlyOrganizationAdmittedCandidate :one
WITH admitted AS MATERIALIZED (
 SELECT a.organization_id,a.issue_id FROM monthly_budget_admissions a
 WHERE a.organization_id=sqlc.arg(organization_id)
)
SELECT EXISTS (
 SELECT 1 FROM admitted a
 JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
 JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
 JOIN issue_claim_facts f ON f.organization_id=i.organization_id AND f.project_id=i.project_id AND f.work_item_id=i.native_id
 WHERE a.organization_id=sqlc.arg(organization_id)

 AND p.profile='native' AND p.deleted_at IS NULL AND i.archived=false
 AND w.dispatchable=true AND w.terminal=false
 AND EXISTS (SELECT 1 FROM queue_entries q WHERE q.organization_id=i.organization_id AND q.project_id=i.project_id AND q.work_item_id=i.native_id AND q.scope=i.project_id)
 AND NOT EXISTS (SELECT 1 FROM workspace_items wi WHERE wi.organization_id=i.organization_id AND wi.project_id=i.project_id AND wi.work_item_id=i.native_id)
 AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.organization_id=i.organization_id AND l.issue_id=i.id AND l.released_at IS NULL AND l.expires_at>sqlc.arg(available_at)::timestamptz)
 AND (NOT p.require_dependencies OR NOT EXISTS (
  SELECT 1 FROM issue_dependencies d
  JOIN issues blocker ON blocker.organization_id=d.organization_id AND blocker.project_id=d.blocker_project_id AND blocker.id=d.blocker_issue_id
  LEFT JOIN project_workflow_states blocker_state ON blocker_state.organization_id=blocker.organization_id AND blocker_state.project_id=blocker.project_id AND blocker_state.name=blocker.workflow_state_name
  WHERE d.organization_id=i.organization_id AND d.dependent_project_id=i.project_id AND d.dependent_issue_id=i.id
  AND (blocker_state.name IS NULL OR blocker_state.terminal=false)
 ))
);

-- name: BillingMonthlyRetainsMachine :one
WITH admitted AS MATERIALIZED (
 SELECT a.organization_id,a.issue_id FROM monthly_budget_admissions a
 JOIN issues cohort_item ON cohort_item.organization_id=a.organization_id AND cohort_item.id=a.issue_id
 WHERE a.organization_id=sqlc.arg(organization_id) AND cohort_item.project_id=sqlc.arg(project_id)
)
SELECT EXISTS (
 SELECT 1 FROM admitted a
 JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id
 JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
 WHERE a.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id)
 AND (SELECT l.machine_id FROM leases l WHERE l.organization_id=i.organization_id AND l.issue_id=i.id ORDER BY l.fencing_token DESC LIMIT 1)=sqlc.arg(machine_id)::text
 AND p.deleted_at IS NULL AND i.archived=false AND w.dispatchable=true AND w.terminal=false
);
