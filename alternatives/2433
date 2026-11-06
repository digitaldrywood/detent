package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ accountstore.Repository = (*Repository)(nil)

func (r *Repository) ReleasePreference(ctx context.Context, organization tracker.OrganizationID, user, membership, principal string) (accountstore.ReleasePreference, error) {
	var preference accountstore.ReleasePreference
	found := false
	err := r.transaction(ctx, organization, corestore.Serving, true, func(s *session) error {
		if _, err := s.authorizeAccountPreference(ctx, user, membership, principal); err != nil {
			return err
		}
		err := s.db.QueryRowContext(ctx, `SELECT version,build_time FROM hosted_release_preferences WHERE organization_id=$1 AND user_id=$2`, organization, user).Scan(&preference.Version, &preference.BuildTime)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapError(ctx, err)
		}
		found = true
		return nil
	})
	if err == nil && !found {
		return accountstore.ReleasePreference{}, sql.ErrNoRows
	}
	return preference, err
}

func (r *Repository) SetReleasePreference(ctx context.Context, organization tracker.OrganizationID, user, membership, principal string, preference accountstore.ReleasePreference) error {
	return r.transaction(ctx, organization, corestore.Serving, false, func(s *session) error {
		if _, err := s.authorizeAccountPreference(ctx, user, membership, principal); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO hosted_release_preferences (organization_id,user_id,version,build_time) VALUES ($1,$2,$3,$4) ON CONFLICT (organization_id,user_id) DO UPDATE SET version=excluded.version,build_time=excluded.build_time WHERE excluded.build_time>=hosted_release_preferences.build_time`, organization, user, preference.Version, preference.BuildTime)
		return mapError(ctx, err)
	})
}

func (r *Repository) DailySummaryPreference(ctx context.Context, organization tracker.OrganizationID, user, membership, principal string) (accountstore.DailySummaryPreference, error) {
	var preference accountstore.DailySummaryPreference
	err := r.transaction(ctx, organization, corestore.Serving, true, func(s *session) error {
		member, err := s.authorizeAccountPreference(ctx, user, membership, principal)
		if err != nil {
			return err
		}
		preference.Recipient = member.Email
		var windowEnd time.Time
		err = s.db.QueryRowContext(ctx, `SELECT enabled,send_minute,time_zone,weekdays,window_end,token FROM daily_summary_preferences WHERE organization_id=$1 AND user_id=$2`, organization, user).Scan(&preference.Enabled, &preference.SendMinute, &preference.TimeZone, &preference.Weekdays, &windowEnd, &preference.Token)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapError(ctx, err)
		}
		preference.Configured = true
		preference.WindowEnd = windowEnd.Unix()
		return nil
	})
	return preference, err
}

func (r *Repository) SetDailySummaryPreference(ctx context.Context, organization tracker.OrganizationID, user, membership, principal string, preference accountstore.DailySummaryPreference, token string, dueAt int64) error {
	return r.transaction(ctx, organization, corestore.Serving, false, func(s *session) error {
		if _, err := s.authorizeAccountPreference(ctx, user, membership, principal); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO daily_summary_preferences (organization_id,user_id,enabled,send_minute,time_zone,weekdays,token,window_end,due_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (user_id,organization_id) DO UPDATE SET enabled=excluded.enabled,send_minute=excluded.send_minute,time_zone=excluded.time_zone,weekdays=excluded.weekdays,window_end=excluded.window_end,due_at=excluded.due_at`, organization, user, preference.Enabled, preference.SendMinute, preference.TimeZone, preference.Weekdays, token, time.Unix(preference.WindowEnd, 0).UTC(), time.Unix(dueAt, 0).UTC())
		return mapError(ctx, err)
	})
}

func (s *session) authorizeAccountPreference(ctx context.Context, user, membership, principal string) (accessstore.Member, error) {
	member, err := s.AccessMember(ctx, user)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return accessstore.Member{}, accessstore.ErrDenied
		}
		return accessstore.Member{}, err
	}
	if !member.Active || member.MembershipID != membership || member.PrincipalID != principal {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	credential, err := s.AccessCredential(ctx, principal)
	if errors.Is(err, corestore.ErrNotFound) {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	if err != nil {
		return accessstore.Member{}, err
	}
	if credential.RevokedAt != nil {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	return member, nil
}

func (r *Repository) UnsubscribeDailySummary(ctx context.Context, organization tracker.OrganizationID, token string) error {
	return r.transaction(ctx, organization, corestore.Serving, false, func(s *session) error {
		result, err := s.db.ExecContext(ctx, `UPDATE daily_summary_preferences SET enabled=false WHERE organization_id=$1 AND token=$2`, organization, token)
		if err != nil {
			return mapError(ctx, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return mapError(ctx, err)
		}
		if count == 0 {
			return corestore.ErrNotFound
		}
		return nil
	})
}
