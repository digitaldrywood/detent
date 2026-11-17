package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerHeartbeatIdentityKey struct{}
type runnerHeartbeatIdentity struct {
	session  *session
	identity *runnerstore.Identity
}
type runnerHeartbeatSession struct {
	*session
	preloaded                             bool
	identity                              runnerstore.Identity
	problems                              []runnerauth.Problem
	problemsChanged                       bool
	repositories                          runnerstore.RepositoryList
	hasRepositories, hasUpdate, hasUrgent bool
}

func (r *Repository) WriteRunnerHeartbeat(ctx context.Context, organization tracker.OrganizationID, credentialID, runnerID string, write func(context.Context, tenantstore.Writer) error) error {
	return r.writeRunnerCommand(ctx, organization, credentialID, runnerID, true, write)
}

func (r *Repository) WriteRunnerCommand(ctx context.Context, organization tracker.OrganizationID, credentialID, runnerID string, write func(context.Context, tenantstore.Writer) error) error {
	return r.writeRunnerCommand(ctx, organization, credentialID, runnerID, false, write)
}

func (r *Repository) writeRunnerCommand(ctx context.Context, organization tracker.OrganizationID, credentialID, runnerID string, preload bool, write func(context.Context, tenantstore.Writer) error) (resultErr error) {
	if _, ok := ctx.Value(readRequestKey{}).(*readRequest); ok {
		return corestore.ErrTenantMismatch
	}
	release, err := r.acquire(ctx, corestore.Serving)
	if err != nil {
		return err
	}
	defer release()
	initial := new(pgx.Batch)
	initial.Queue(runnerIdentitySelect+`token_id=$2 FOR UPDATE`, organization, credentialID)
	if preload {
		initial.Queue(`SELECT ARRAY[COALESCE(r.request_id,''),COALESCE(r.reason,'')],ARRAY[r.runner_id IS NOT NULL,COALESCE(r.pending,false),
 EXISTS(SELECT 1 FROM runner_update_desired WHERE organization_id=$1 AND runner_id=$2) OR EXISTS(SELECT 1 FROM runner_update_observations WHERE organization_id=$1 AND runner_id=$2),
 EXISTS(SELECT 1 FROM runner_urgent_updates WHERE organization_id=$1)],ARRAY[r.listed_at]
 FROM (SELECT 1) seed LEFT JOIN runner_repository_lists r ON r.organization_id=$1 AND r.runner_id=$2
 UNION ALL
 SELECT ARRAY[code,project_id,subject,check_name,error_output,fix_command,message,fix_hint],ARRAY[cached_capabilities],ARRAY[reported_at,first_seen]
 FROM (SELECT * FROM runner_problem_reports WHERE organization_id=$1 AND runner_id=$2 ORDER BY code,project_id,subject,check_name LIMIT 129) problems`, organization, runnerID)
	}
	tx, results, err := r.runtime.pools.BeginBatch(ctx, pgtransport.Scope{Organization: organization}, pgtransport.Options{}, initial)
	if err != nil {
		return mapError(ctx, err)
	}
	defer func() { resultErr = errors.Join(resultErr, mapError(ctx, tx.Rollback(ctx))) }()
	identity, err := scanRunnerIdentity(ctx, results.QueryRow())
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			err = accessstore.ErrDenied
		}
		return errors.Join(err, results.Close())
	}
	if identity.RunnerID != runnerID {
		return errors.Join(accessstore.ErrDenied, results.Close())
	}
	db := timedQueries{DBTX: tx}
	h := &runnerHeartbeatSession{session: &session{organization: organization, leaseClock: r.leaseClock, db: db, queries: pgquery.New(db), flushesBoardDeltas: true}, identity: identity, preloaded: preload}
	if preload {
		if err := h.readHeartbeatRows(ctx, results); err != nil {
			return errors.Join(err, results.Close())
		}
	}
	if err := results.Close(); err != nil {
		return mapError(ctx, err)
	}
	h.credentials = make(map[string]accessstore.Credential)
	if _, err := h.AccessCredential(ctx, credentialID); err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			err = accessstore.ErrDenied
		}
		return err
	}
	h.identity.ProjectIDs, err = h.runnerRoutingProjects(ctx, &h.identity, runnerstore.MaxRunnerProjectList)
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, runnerHeartbeatIdentityKey{}, runnerHeartbeatIdentity{session: h.session, identity: &h.identity})
	if err := write(ctx, h); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	hasDelivery := len(h.boardDeltas) != 0
	if err := h.flushBoardDeltas(ctx); err != nil {
		return err
	}
	err = tx.Commit(ctx)
	if err == nil && hasDelivery {
		r.notifyBoardCommit()
	}
	return mapError(ctx, err)
}

func (h *runnerHeartbeatSession) readHeartbeatRows(ctx context.Context, results pgx.BatchResults) error {
	rows, err := results.Query()
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var values []string
		var flags []bool
		var times []*time.Time
		if err := rows.Scan(&values, &flags, &times); err != nil {
			rows.Close()
			return mapError(ctx, err)
		}
		if len(flags) == 4 {
			h.hasRepositories, h.hasUpdate, h.hasUrgent = flags[0], flags[2], flags[3]
			h.repositories = runnerstore.RepositoryList{OrganizationID: h.organization, RunnerID: h.identity.RunnerID, RequestID: values[0], Reason: values[1], Pending: flags[1], ListedAt: times[0]}
		} else {
			if len(h.problems) == maxRunnerProblemRows {
				rows.Close()
				return corestore.ErrInvalidPage
			}
			h.problems = append(h.problems, runnerauth.Problem{Code: values[0], ProjectID: values[1], Subject: values[2], Check: values[3], ErrorOutput: values[4], FixCommand: values[5], Message: values[6], FixHint: values[7], ReportedAt: *times[0], FirstSeen: *times[1], CachedCapabilities: flags[0]})
		}
	}
	return mapError(ctx, rows.Err())
}

func (h *runnerHeartbeatSession) RunnerProjects(_ context.Context, id string, limit int) ([]tracker.ProjectID, error) {
	if id != h.identity.RunnerID {
		return nil, corestore.ErrNotFound
	}
	if limit < 1 || limit > runnerstore.MaxRunnerProjectList {
		return nil, corestore.ErrInvalidPage
	}
	if len(h.identity.ProjectIDs) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	return append([]tracker.ProjectID{}, h.identity.ProjectIDs...), nil
}
func (h *runnerHeartbeatSession) RunnerProblems(ctx context.Context, id string) ([]runnerauth.Problem, error) {
	if !h.preloaded {
		return h.session.RunnerProblems(ctx, id)
	}
	if id != h.identity.RunnerID {
		return nil, corestore.ErrNotFound
	}
	if h.problemsChanged {
		return h.session.RunnerProblems(ctx, id)
	}
	return append([]runnerauth.Problem{}, h.problems...), nil
}
func (h *runnerHeartbeatSession) RunnerRepositoryList(ctx context.Context, id string) (runnerstore.RepositoryList, error) {
	if !h.preloaded {
		return h.session.RunnerRepositoryList(ctx, id)
	}
	if id != h.identity.RunnerID || !h.hasRepositories {
		return runnerstore.RepositoryList{}, corestore.ErrNotFound
	}
	return h.repositories, nil
}
func (h *runnerHeartbeatSession) RunnerUpdateState(ctx context.Context, id string) (runnerstore.UpdateState, error) {
	if h.preloaded && id == h.identity.RunnerID && !h.hasUpdate {
		return runnerstore.UpdateState{OrganizationID: h.organization, RunnerID: id}, nil
	}
	return h.session.RunnerUpdateState(ctx, id)
}
func (h *runnerHeartbeatSession) RunnerUrgentUpdate(ctx context.Context) (runnerstore.UrgentUpdate, error) {
	if h.preloaded && !h.hasUrgent {
		return runnerstore.UrgentUpdate{OrganizationID: h.organization}, nil
	}
	return h.session.RunnerUrgentUpdate(ctx)
}
func (h *runnerHeartbeatSession) ReplaceRunnerUpdateObservation(ctx context.Context, id string, report *runnerauth.UpdateObservation) error {
	if report == nil && h.preloaded && id == h.identity.RunnerID && !h.hasUpdate {
		return nil
	}
	if err := h.session.ReplaceRunnerUpdateObservation(ctx, id, report); err != nil {
		return err
	}
	if id == h.identity.RunnerID {
		h.hasUpdate = true
	}
	return nil
}

func (h *runnerHeartbeatSession) LockRunnerUpdate(_ context.Context, id string) error {
	if id != h.identity.RunnerID {
		return corestore.ErrNotFound
	}
	return nil
}

func (h *runnerHeartbeatSession) UpdateRunnerIdentity(ctx context.Context, row runnerstore.Identity) error {
	if err := h.session.UpdateRunnerIdentity(ctx, row); err != nil {
		return err
	}
	if row.RunnerID == h.identity.RunnerID {
		h.identity.DisplayName = row.DisplayName
		h.identity.CapacityLimit = row.CapacityLimit
		h.identity.ReportedCapacity = row.ReportedCapacity
		h.identity.OS = row.OS
		h.identity.Architecture = row.Architecture
		h.identity.LastHeartbeatAt = pgtransport.Timestamp(row.LastHeartbeatAt)
		h.identity.ExpiresAt = pgtransport.Timestamp(row.ExpiresAt)
	}
	return nil
}

func (h *runnerHeartbeatSession) SetRunnerUpdateDesired(ctx context.Context, id string, revision int64, requestID string) error {
	if err := h.session.SetRunnerUpdateDesired(ctx, id, revision, requestID); err != nil {
		return err
	}
	if id == h.identity.RunnerID {
		h.identity.Revision = revision + 1
		h.hasUpdate = true
	}
	return nil
}

func (h *runnerHeartbeatSession) UpdateRunnerCapacityConfig(ctx context.Context, id string, config runnerauth.CapacityConfig) error {
	if err := h.session.UpdateRunnerCapacityConfig(ctx, id, config); err != nil {
		return err
	}
	if id == h.identity.RunnerID {
		config.ObservedAt = pgtransport.Timestamp(config.ObservedAt)
		h.identity.CapacityConfig = &config
	}
	return nil
}

func (h *runnerHeartbeatSession) ReplaceRunnerProblems(ctx context.Context, id string, problems []runnerauth.Problem) error {
	if err := h.session.ReplaceRunnerProblems(ctx, id, problems); err != nil {
		return err
	}
	if id == h.identity.RunnerID {
		h.problemsChanged = true
	}
	return nil
}

func (h *runnerHeartbeatSession) UpdateRunnerRouting(ctx context.Context, revision int64, row runnerstore.Identity) error {
	if err := h.session.UpdateRunnerRouting(ctx, revision, row); err != nil {
		return err
	}
	if row.RunnerID != h.identity.RunnerID {
		return nil
	}
	current, err := scanRunnerIdentity(ctx, h.db.QueryRow(ctx, runnerIdentitySelect+`runner_id=$2`, h.organization, row.RunnerID))
	if err != nil {
		return err
	}
	current.ProjectIDs, err = h.runnerRoutingProjects(ctx, &current, runnerstore.MaxRunnerProjectList)
	if err != nil {
		return err
	}
	h.identity = current
	return nil
}

func (h *runnerHeartbeatSession) RemoveRunnerIdentity(ctx context.Context, id string, at time.Time) error {
	if err := h.session.RemoveRunnerIdentity(ctx, id, at); err != nil {
		return err
	}
	if id == h.identity.RunnerID {
		at = pgtransport.Timestamp(at)
		h.identity.RemovedAt = &at
		h.identity.Revision++
	}
	return nil
}

type runnerHeartbeatSnapshotKey struct{}

type runnerHeartbeatSnapshot struct {
	*runnerHeartbeatSession
	machine  leasestore.Machine
	project  tracker.ProjectID
	allowed  bool
	now      time.Time
	policyID string
	slots    []runnerauth.ClaimSlot
}

type runnerHeartbeatCredentialRow struct {
	pgx.Row
	credential *accessstore.Credential
}

func (r runnerHeartbeatCredentialRow) Scan(dest ...any) error {
	c := r.credential
	dest = append(dest, &c.OrganizationID, &c.ID, &c.Name, &c.Hash, &c.Fingerprint, &c.Scope, &c.NativeOnly, &c.CreatedAt, &c.UpdatedAt, &c.RotatedAt, &c.RevokedAt, &c.ExpiresAt, &c.LastUsedAt, &c.UserID, &c.MembershipID, &c.KeyScope, &c.ProjectAccess, &c.Kind)
	return r.Row.Scan(dest...)
}

func (r *Repository) ReadRunnerHeartbeatSnapshot(ctx context.Context, organization tracker.OrganizationID, credentialID, runnerID string, project tracker.ProjectID, clock func() time.Time, read func(context.Context, tenantstore.Reader, time.Time) error) error {
	if clock == nil || project == "" || credentialID == "" || runnerID == "" {
		return corestore.ErrInvalidPage
	}
	if _, nested := ctx.Value(readRequestKey{}).(*readRequest); nested {
		return corestore.ErrTenantMismatch
	}
	return r.transactionIsolation(ctx, organization, corestore.Serving, true, pgx.RepeatableRead, func(s *session) error {
		now := clock().UTC()
		if now.IsZero() {
			return corestore.ErrInvalidPage
		}
		columns, _, ok := strings.Cut(strings.TrimPrefix(runnerIdentitySelect, "SELECT "), " FROM ")
		if !ok {
			return corestore.ErrInvalidPage
		}
		query := "SELECT r." + strings.ReplaceAll(columns, ",", ",r.") + `,c.organization_id,c.id,c.name,c.token_hash,c.token_fingerprint,c.scope,c.native_only,c.created_at,c.updated_at,c.rotated_at,c.revoked_at,c.expires_at,c.last_used_at,c.hosted_user_id,c.hosted_membership_id,c.operator_key_scope,c.operator_project_access,c.key_kind FROM runner_identities r JOIN api_tokens c ON c.organization_id=r.organization_id AND c.id=r.token_id WHERE r.organization_id=$1 AND r.token_id=$2 AND r.runner_id=$3`
		var credential accessstore.Credential
		identity, err := scanRunnerIdentity(ctx, runnerHeartbeatCredentialRow{Row: s.db.QueryRow(ctx, query, organization, credentialID, runnerID), credential: &credential})
		if err != nil {
			return err
		}
		s.credentials = map[string]accessstore.Credential{credentialID: credential}
		h := &runnerHeartbeatSnapshot{runnerHeartbeatSession: &runnerHeartbeatSession{session: s, identity: identity, preloaded: true}, project: project, now: now}
		if err := h.readSnapshotRows(ctx); err != nil {
			return err
		}
		snapshotCtx := context.WithValue(ctx, runnerHeartbeatIdentityKey{}, runnerHeartbeatIdentity{session: s, identity: &h.identity})
		snapshotCtx = context.WithValue(snapshotCtx, runnerHeartbeatSnapshotKey{}, h)
		return read(snapshotCtx, h, now)
	})
}

func (h *runnerHeartbeatSnapshot) Machine(ctx context.Context, id string) (leasestore.Machine, error) {
	if id == h.machine.ID {
		return h.machine, nil
	}
	return h.session.Machine(ctx, id)
}

func (h *runnerHeartbeatSnapshot) RunnerProjectAuthorized(ctx context.Context, runner string, project tracker.ProjectID, now time.Time) (bool, error) {
	if runner == h.identity.RunnerID && project == h.project && now.Equal(h.now) {
		return h.allowed, nil
	}
	return h.session.RunnerProjectAuthorized(ctx, runner, project, now)
}

func (h *runnerHeartbeatSnapshot) readSnapshotRows(ctx context.Context) error {
	query := `WITH routing AS MATERIALIZED (` + runnerRoutingProjectsSQL + `), slots AS MATERIALIZED (
 SELECT l.lease_id,COALESCE(a.runner_id,'') AS runner_id FROM leases l
 LEFT JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id
 WHERE l.organization_id=$1 AND l.machine_id=$8 AND l.released_at IS NULL AND l.expires_at>$9
 ORDER BY l.lease_id LIMIT 10001
 ) SELECT kind,keys,counts,flags,instants FROM (
 SELECT kind,ARRAY[key,value] AS keys,ARRAY[number::bigint] AS counts,ARRAY[]::boolean[] AS flags,ARRAY[]::timestamptz[] AS instants FROM routing
 UNION ALL
 SELECT 5,ARRAY[m.id,m.hostname,m.display_name,m.version,m.sprite_name,COALESCE(policy.policy_id,''),COALESCE(repositories.request_id,''),COALESCE(repositories.reason,'')],ARRAY[m.capacity],
 ARRAY[EXISTS (` + strings.NewReplacer("$3", "$7", "$4", "$9").Replace(runnerProjectAuthorizedSQL) + `),
 repositories.runner_id IS NOT NULL,COALESCE(repositories.pending,false),
 EXISTS(SELECT 1 FROM runner_update_desired WHERE organization_id=$1 AND runner_id=$2) OR EXISTS(SELECT 1 FROM runner_update_observations WHERE organization_id=$1 AND runner_id=$2),
 EXISTS(SELECT 1 FROM runner_urgent_updates WHERE organization_id=$1)],ARRAY[m.sprite_woken_at,m.sprite_wake_failed_at,repositories.listed_at]
 FROM machines m LEFT JOIN runner_repository_lists repositories ON repositories.organization_id=$1 AND repositories.runner_id=$2
 LEFT JOIN (SELECT r.policy_id FROM project_policies p JOIN policy_revisions r ON r.organization_id=p.organization_id AND r.project_id=p.project_id AND r.policy_id=p.policy_id WHERE p.organization_id=$1 AND p.project_id=$7) policy ON true
 WHERE m.organization_id=$1 AND m.id=$8
 UNION ALL SELECT 6,ARRAY[lease_id,runner_id],ARRAY[]::bigint[],ARRAY[]::boolean[],ARRAY[]::timestamptz[] FROM slots
 ) snapshot_rows ORDER BY kind,keys[1],counts[1]`
	rows, err := h.db.Query(ctx, query, h.organization, h.identity.RunnerID, h.identity.TokenID, h.identity.Scope, runnerstore.MaxRunnerProjectList+1, runnerstore.MaxRunnerProjectRanks+1, h.project, string(h.identity.MachineID), h.now)
	if err != nil {
		return mapError(ctx, err)
	}
	defer rows.Close()
	h.identity.Availability.Windows = nil
	h.identity.Availability.WindowSlots = map[string]int{}
	h.identity.ProjectCapacityLimits = map[tracker.ProjectID]int{}
	h.identity.ProjectRanks = map[tracker.ProjectID]int{}
	h.identity.ProjectIDs = []tracker.ProjectID{}
	foundMachine := false
	for rows.Next() {
		var kind int
		var keys []string
		var counts []int64
		var flags []bool
		var instants []*time.Time
		if err := rows.Scan(&kind, &keys, &counts, &flags, &instants); err != nil {
			return mapError(ctx, err)
		}
		switch kind {
		case 0, 1, 2, 3, 4:
			if len(keys) != 2 || len(counts) != 1 {
				return corestore.ErrInvalidPage
			}
			number := int(counts[0])
			switch kind {
			case 0:
				if number != len(h.identity.Availability.Windows) {
					return corestore.ErrInvalidPage
				}
				h.identity.Availability.Windows = append(h.identity.Availability.Windows, keys[1])
			case 1:
				h.identity.Availability.WindowSlots[keys[0]] = number
			case 2:
				h.identity.ProjectCapacityLimits[tracker.ProjectID(keys[0])] = number
			case 3:
				h.identity.ProjectRanks[tracker.ProjectID(keys[0])] = number
				if len(h.identity.ProjectRanks) > runnerstore.MaxRunnerProjectRanks {
					return runnerstore.ErrInvalidRecord
				}
			case 4:
				h.identity.ProjectIDs = append(h.identity.ProjectIDs, tracker.ProjectID(keys[0]))
				if len(h.identity.ProjectIDs) > runnerstore.MaxRunnerProjectList {
					return runnerstore.ErrLimitExceeded
				}
			}
		case 5:
			if foundMachine || len(keys) != 8 || len(counts) != 1 || len(flags) != 5 || len(instants) != 3 {
				return corestore.ErrInvalidPage
			}
			foundMachine = true
			h.machine = leasestore.Machine{OrganizationID: h.organization, ID: keys[0], Hostname: keys[1], DisplayName: keys[2], Version: keys[3], SpriteName: keys[4], Capacity: counts[0], SpriteWokenAt: instants[0], SpriteWakeFailedAt: instants[1]}
			h.policyID = keys[5]
			h.allowed = flags[0]
			h.hasRepositories = flags[1]
			h.hasUpdate = flags[3]
			h.hasUrgent = flags[4]
			h.repositories = runnerstore.RepositoryList{OrganizationID: h.organization, RunnerID: h.identity.RunnerID, RequestID: keys[6], Reason: keys[7], Pending: flags[2], ListedAt: instants[2]}
		case 6:
			if len(keys) != 2 {
				return corestore.ErrInvalidPage
			}
			h.slots = append(h.slots, runnerauth.ClaimSlot{ID: tracker.LeaseID(keys[0]), RunnerID: keys[1]})
			if len(h.slots) > 10000 {
				return corestore.ErrInvalidPage
			}
		default:
			return corestore.ErrInvalidPage
		}
	}
	if err := mapError(ctx, rows.Err()); err != nil {
		return err
	}
	if !foundMachine {
		return corestore.ErrNotFound
	}
	return nil
}
