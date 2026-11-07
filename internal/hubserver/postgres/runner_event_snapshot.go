package postgres

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
)

type runFact[T any] struct {
	value T
	err   error
}

func queueRunFact[T any](b *pgx.Batch, fact *runFact[T], query string, args ...any) {
	b.Queue(query, args...).Query(func(rows pgx.Rows) error {
		fact.value, fact.err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[T])
		if errors.Is(fact.err, pgx.ErrNoRows) {
			fact.err = corestore.ErrNotFound
			return nil
		}
		return fact.err
	})
}

type runnerEventSession struct {
	tenantstore.Writer
	sessions        runnersession.Writer
	session         *session
	command         eventstore.RunCommand
	project         runFact[pgquery.Project]
	item            runFact[pgquery.Issue]
	lease           runFact[pgquery.Lease]
	attempt         runFact[pgquery.NativeAttempt]
	latest          runFact[pgquery.NativeAttempt]
	machine         runFact[pgquery.Machine]
	policy          runFact[pgquery.PolicyGetProjectCurrentRow]
	pinned          runFact[policystore.LeasePolicy]
	receipt         runFact[eventstore.Receipt]
	latestEvent     runFact[pgquery.CollaborationEvent]
	events          map[string]eventstore.Event
	pending         *pgx.Batch
	attemptReceipts map[int64]string
	runnerAllowed   bool
	owner           string
	now             time.Time
	sessionRecord   runFact[runnersession.Record]
}

func newRunnerEventFacts(s *session, writer tenantstore.Writer, c eventstore.RunCommand) (*runnerEventSession, *pgx.Batch) {
	f := &runnerEventSession{Writer: writer, session: s, command: c, events: map[string]eventstore.Event{}, sessions: s, pending: new(pgx.Batch), attemptReceipts: map[int64]string{}}
	b := new(pgx.Batch)
	lock := ""
	if !s.readOnly {
		lock = " FOR SHARE"
		if c.Completion || c.Operation != "" {
			lock = " FOR UPDATE"
		}
	}
	queueRunFact(b, &f.project, `SELECT * FROM projects WHERE organization_id=$1 AND id=$2`+lock, s.organization, c.Project)
	lock = ""
	if !s.readOnly {
		lock = " FOR UPDATE"
	}
	queueRunFact(b, &f.item, `SELECT * FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=COALESCE(NULLIF($3,''),(SELECT issue_id FROM runner_sessions WHERE organization_id=$1 AND project_id=$2 AND id=$4))`+lock, s.organization, c.Project, c.WorkItem, c.SessionID)
	queueRunFact(b, &f.lease, `SELECT * FROM leases WHERE organization_id=$1 AND lease_id=$2`+lock, s.organization, c.LeaseID)
	b.Queue(`SELECT clock_timestamp()`).QueryRow(func(row pgx.Row) error { return row.Scan(&f.now) })
	queueRunFact(b, &f.attempt, `SELECT * FROM native_attempts WHERE organization_id=$1 AND id=$2`, s.organization, c.AttemptID)
	queueRunFact(b, &f.latest, `SELECT a.* FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND i.project_id=$2 AND i.native_id=COALESCE(NULLIF($3,''),(SELECT issue_id FROM runner_sessions WHERE organization_id=$1 AND project_id=$2 AND id=$4)) ORDER BY a.fencing_token DESC LIMIT 1`, s.organization, c.Project, c.WorkItem, c.SessionID)
	queueRunFact(b, &f.machine, `SELECT m.* FROM machines m JOIN leases l ON l.organization_id=m.organization_id AND l.machine_id=m.id WHERE l.organization_id=$1 AND l.lease_id=$2`, s.organization, c.LeaseID)
	queueRunFact(b, &f.pinned, `SELECT organization_id,project_id,lease_id,policy_id,bound_at FROM lease_policy_bindings WHERE organization_id=$1 AND lease_id=$2`, s.organization, c.LeaseID)
	queueRunFact(b, &f.policy, `SELECT r.organization_id,r.project_id,r.policy_id,COALESCE(a.approved_by,r.approved_by) AS approved_by,COALESCE(a.approved_at,r.approved_at) AS approved_at,r.payload_object_key,r.payload_sha256,r.payload_bytes,r.payload_content_bytes,r.gate_github_pull_request FROM project_policies p JOIN policy_revisions r ON r.organization_id=p.organization_id AND r.project_id=p.project_id AND r.policy_id=p.policy_id LEFT JOIN policy_revision_approvals a ON a.organization_id=r.organization_id AND a.project_id=r.project_id AND a.policy_id=r.policy_id WHERE p.organization_id=$1 AND p.project_id=$2`, s.organization, c.Project)
	queueRunFact(b, &f.latestEvent, `SELECT * FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND attempt_id=$3 AND attempt_id<>'' ORDER BY sequence DESC LIMIT 1`, s.organization, c.Project, c.AttemptID)
	b.Queue(`SELECT requested.organization_id,requested.actor_id,requested.operation,requested.command_key,COALESCE(c.request_hash,state.request_hash),COALESCE(c.created_at,state.created_at),COALESCE(c.payload_object_key,''),COALESCE(c.payload_sha256,''),COALESCE(c.payload_bytes,0),COALESCE(c.payload_content_bytes,0),COALESCE(state.response_claimed,false),COALESCE(state.response_pending,false),COALESCE(state.response_request_id,''),COALESCE(state.response_expired,false),COALESCE(state.response_replay_until_ns,0),COALESCE(state.response_reconciliation_hash,'')
FROM (SELECT $1::text organization_id,$2::text actor_id,$3::text operation,$4::text command_key) requested
LEFT JOIN native_commands c USING(organization_id,actor_id,operation,command_key)
LEFT JOIN native_command_states state USING(organization_id,actor_id,operation,command_key)
WHERE c.command_key IS NOT NULL OR state.command_key IS NOT NULL`, s.organization, c.ActorID, c.Operation, c.Key).QueryRow(func(row pgx.Row) error {
		f.receipt.err = row.Scan(&f.receipt.value.OrganizationID, &f.receipt.value.ActorID, &f.receipt.value.Operation, &f.receipt.value.CommandKey, &f.receipt.value.RequestHash, &f.receipt.value.CreatedAt, &f.receipt.value.PayloadObjectKey, &f.receipt.value.PayloadSHA256, &f.receipt.value.PayloadBytes, &f.receipt.value.PayloadContentBytes, &f.receipt.value.ResponseClaimed, &f.receipt.value.ResponsePending, &f.receipt.value.ResponseRequestID, &f.receipt.value.ResponseExpired, &f.receipt.value.ResponseReplayUntilNs, &f.receipt.value.ResponseReconciliationHash)
		if errors.Is(f.receipt.err, pgx.ErrNoRows) {
			f.receipt.err = corestore.ErrNotFound
			return nil
		}
		return f.receipt.err
	})
	b.Queue(`SELECT sequence,request_hash FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2 AND sequence=ANY($3::bigint[])`, s.organization, c.AttemptID, c.Sequences).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var sequence int64
			var hash string
			if err := rows.Scan(&sequence, &hash); err != nil {
				return err
			}
			f.attemptReceipts[sequence] = hash
		}
		return rows.Err()
	})
	b.Queue(`SELECT EXISTS (
SELECT 1 FROM runner_identities r
JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id
JOIN projects p ON p.organization_id=r.organization_id AND p.id=$3 AND p.deleted_at IS NULL
WHERE r.organization_id=$1 AND r.runner_id=(SELECT runner_id FROM runner_lease_assignments WHERE organization_id=$1 AND lease_id=$2) AND r.removed_at IS NULL
AND r.expires_at>clock_timestamp() AND t.scope='worker' AND t.key_kind='service' AND t.revoked_at IS NULL
AND t.created_at<=clock_timestamp() AND (t.expires_at IS NULL OR t.expires_at>clock_timestamp())
AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=p.id)))`, s.organization, c.LeaseID, c.Project).QueryRow(func(row pgx.Row) error { return row.Scan(&f.runnerAllowed) })
	if s.tokenGrants == nil {
		s.tokenGrants = map[projectAccessKey]accessstore.TokenGrant{}
	}
	b.Queue(`SELECT g.organization_id,g.token_id,g.project_id FROM token_grants g JOIN runner_lease_assignments a ON a.organization_id=g.organization_id JOIN runner_identities r ON r.organization_id=a.organization_id AND r.runner_id=a.runner_id AND r.token_id=g.token_id WHERE a.organization_id=$1 AND a.lease_id=$2 AND g.project_id=$3`+s.accessLock(), s.organization, c.LeaseID, c.Project).QueryRow(func(row pgx.Row) error {
		var grant accessstore.TokenGrant
		err := row.Scan(&grant.OrganizationID, &grant.TokenID, &grant.ProjectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			s.tokenGrants[projectAccessKey{principal: grant.TokenID, project: grant.ProjectID}] = grant
		}
		return err
	})
	b.Queue(`SELECT runner_id FROM runner_lease_assignments WHERE organization_id=$1 AND lease_id=$2`, s.organization, c.LeaseID).QueryRow(func(row pgx.Row) error {
		err := row.Scan(&f.owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	b.Queue(`SELECT * FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND id=ANY($4::text[])`, s.organization, c.Project, c.WorkItem, c.EventIDs).Query(func(rows pgx.Rows) error {
		values, err := pgx.CollectRows(rows, pgx.RowToStructByName[pgquery.CollaborationEvent])
		for _, v := range values {
			f.events[v.ID] = eventRecord(v)
		}
		return err
	})
	if c.SessionID > 0 {
		b.Queue(`SELECT `+runnerSessionColumns+` FROM runner_sessions WHERE organization_id=$1 AND project_id=$2 AND id=$3`, s.organization, c.Project, c.SessionID).QueryRow(func(row pgx.Row) error {
			f.sessionRecord.value, f.sessionRecord.err = scanRunnerSession(row)
			if errors.Is(f.sessionRecord.err, pgx.ErrNoRows) {
				f.sessionRecord.err = corestore.ErrNotFound
				return nil
			}
			return f.sessionRecord.err
		})
	}
	return f, b
}
