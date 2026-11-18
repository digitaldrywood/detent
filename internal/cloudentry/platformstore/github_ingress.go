package platformstore

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/githuboutbound"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

type GitHubIngress struct {
	DeliveryID, EventType, Repository, Digest, ObjectKey string
}

func (g GitHubIngress) valid() bool {
	return strings.TrimSpace(g.DeliveryID) != "" && len(g.DeliveryID) <= 256 && strings.TrimSpace(g.EventType) != "" && len(g.EventType) <= 100 && g.ObjectKey != "" && artifact.ValidHash(g.Digest, 64)
}

func (r *Repository) BeginGitHubIngress(ctx context.Context, g *GitHubIngress, now time.Time) (bool, error) {
	if g == nil || !g.valid() || now.IsZero() {
		return false, ErrInvalid
	}
	var accepted bool
	var key string
	err := r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO platform.github_webhook_ingress AS i
			(delivery_id,event_type,repository,payload_digest,payload_object_key,last_received_at,next_attempt_at,state)
			VALUES($1,$2,$3,$4,$5,$6,$7,'staging') ON CONFLICT(delivery_id) DO UPDATE
			SET last_received_at=GREATEST(i.last_received_at,excluded.last_received_at)
			WHERE i.event_type=excluded.event_type AND i.repository=excluded.repository AND i.payload_digest=excluded.payload_digest
			RETURNING i.state<>'staging',i.payload_object_key`, g.DeliveryID, g.EventType, g.Repository, g.Digest, g.ObjectKey, now, now.Add(30*time.Second)).Scan(&accepted, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			return corestore.ErrRevisionConflict
		}
		return err
	})
	if err == nil && !accepted {
		g.ObjectKey = key
	}
	return accepted, err
}

func (r *Repository) AcceptGitHubIngress(ctx context.Context, g GitHubIngress, now time.Time) (bool, error) {
	if !g.valid() || now.IsZero() {
		return false, ErrInvalid
	}
	var promoted bool
	err := r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE platform.github_webhook_ingress SET state='routing',attempts=0,next_attempt_at=$6,dispatch_order=DEFAULT
		WHERE delivery_id=$1 AND event_type=$2 AND repository=$3 AND payload_digest=$4 AND payload_object_key=$5 AND state='staging'`,
			g.DeliveryID, g.EventType, g.Repository, g.Digest, g.ObjectKey, now)
		if err != nil {
			return err
		}
		promoted = tag.RowsAffected() == 1
		if promoted {
			return nil
		}
		var matches, accepted bool
		err = tx.QueryRow(ctx, `SELECT event_type=$2 AND repository=$3 AND payload_digest=$4, state<>'staging'
			FROM platform.github_webhook_ingress WHERE delivery_id=$1`, g.DeliveryID, g.EventType, g.Repository, g.Digest).Scan(&matches, &accepted)
		if err != nil {
			return err
		}
		if !matches {
			return corestore.ErrRevisionConflict
		}
		if !accepted {
			return corestore.ErrNotFound
		}
		return nil
	})
	return promoted, err
}

func (r *Repository) LatestGitHubIngress(ctx context.Context) (time.Time, error) {
	var latest time.Time
	err := r.read(ctx, corestore.Serving, func(q pgquery.DBTX) error {
		err := q.QueryRow(ctx, `SELECT last_received_at FROM platform.github_webhook_ingress WHERE state<>'staging' ORDER BY last_received_at DESC LIMIT 1`).Scan(&latest)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return latest, err
}

func (r *Repository) GitHubIngressReleased(ctx context.Context, deliveryID, digest, key string) (bool, error) {
	var released bool
	err := r.read(ctx, corestore.Background, func(q pgquery.DBTX) error {
		return q.QueryRow(ctx, `SELECT NOT EXISTS(SELECT FROM platform.github_webhook_ingress
			WHERE delivery_id=$1 AND payload_digest=$2 AND payload_object_key=$3 AND state<>'completed')`, deliveryID, digest, key).Scan(&released)
	})
	return released, err
}

type GitHubFanoutPage struct {
	GitHubIngress
	Targets                         []hubmaintenance.Tenant
	BindingProject                  string
	Staging, Cleanup                bool
	afterOrganization, afterProject string
	order, attempts                 int64
	exhausted, passFailed           bool
}

func (r *Repository) NextGitHubFanout(ctx context.Context, now time.Time, limit int) (*GitHubFanoutPage, error) {
	if now.IsZero() || limit < 1 || limit > hubmaintenance.TenantLimit {
		return nil, ErrInvalid
	}
	p := &GitHubFanoutPage{}
	err := r.read(ctx, corestore.Background, func(q pgquery.DBTX) error {
		err := q.QueryRow(ctx, `SELECT delivery_id,event_type,repository,payload_digest,payload_object_key,
		state='staging',state='cleanup',after_organization,after_project,dispatch_order,attempts,pass_failed
		FROM platform.github_webhook_ingress WHERE state IN ('staging','routing','cleanup') AND next_attempt_at<=$1
		ORDER BY next_attempt_at,dispatch_order LIMIT 1`, now).Scan(&p.DeliveryID, &p.EventType, &p.Repository, &p.Digest, &p.ObjectKey, &p.Staging, &p.Cleanup, &p.afterOrganization, &p.afterProject, &p.order, &p.attempts, &p.passFailed)
		if err != nil || p.Staging || p.Cleanup {
			return err
		}
		return p.readBindings(ctx, q, limit)
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (p *GitHubFanoutPage) readBindings(ctx context.Context, q pgquery.DBTX, limit int) error {
	rows, err := q.Query(ctx, `WITH bindings AS MATERIALIZED (
		(SELECT organization_id,project_id FROM public.project_settings WHERE repository<>'' AND lower(repository)=$1
		 AND (organization_id,project_id)>($2,$3) ORDER BY organization_id,project_id LIMIT $4)
		UNION
		(SELECT organization_id,project_id FROM public.project_settings WHERE checkout_repository<>'' AND lower(checkout_repository)=$1
		 AND (organization_id,project_id)>($2,$3) ORDER BY organization_id,project_id LIMIT $4)
		ORDER BY organization_id,project_id LIMIT $4
	)
	SELECT b.organization_id,b.project_id,COALESCE(o.generation,0)
	FROM bindings b
	JOIN platform.github_webhook_ingress i ON i.delivery_id=$5 AND i.dispatch_order=$8
	JOIN public.project_settings s ON s.organization_id=b.organization_id AND s.project_id=b.project_id
	LEFT JOIN public.projects p ON p.organization_id=b.organization_id AND p.id=b.project_id AND p.profile='native' AND p.deleted_at IS NULL
		AND (p.github_binding_xid IS NULL OR pg_visible_in_snapshot(p.github_binding_xid,i.accepted_snapshot))
	LEFT JOIN public.github_webhook_receipts receipt ON receipt.organization_id=b.organization_id AND receipt.delivery_id=$5
		AND receipt.binding_project=b.project_id AND receipt.event_type=$6 AND receipt.repository=$1 AND receipt.payload_digest=$7
	LEFT JOIN platform.organizations o ON o.id=p.organization_id AND o.state='ready' AND receipt.delivery_id IS NULL
		AND (o.github_binding_xid IS NULL OR pg_visible_in_snapshot(o.github_binding_xid,i.accepted_snapshot))
		AND ((lower(s.repository)=$1 AND (s.github_repository_xid IS NULL OR pg_visible_in_snapshot(s.github_repository_xid,i.accepted_snapshot)))
		  OR (lower(s.checkout_repository)=$1 AND (s.github_checkout_xid IS NULL OR pg_visible_in_snapshot(s.github_checkout_xid,i.accepted_snapshot))))
	ORDER BY b.organization_id,b.project_id`, p.Repository, p.afterOrganization, p.afterProject, limit+1, p.DeliveryID, p.EventType, p.Digest, p.order)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > limit || p.BindingProject != "" {
			return nil
		}
		var generation int64
		if err := rows.Scan(&p.afterOrganization, &p.afterProject, &generation); err != nil {
			return err
		}
		if generation > 0 {
			p.BindingProject = p.afterProject
			p.Targets = append(p.Targets, hubmaintenance.Tenant{Organization: p.afterOrganization, Generation: generation})
		}
	}
	p.exhausted = true
	return rows.Err()
}

func (r *Repository) ReleaseGitHubStaging(ctx context.Context, page *GitHubFanoutPage) error {
	if page == nil || !page.Staging || page.order <= 0 {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Background, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM platform.github_webhook_ingress
			WHERE delivery_id=$1 AND state='staging' AND dispatch_order=$2 AND payload_object_key=$3`, page.DeliveryID, page.order, page.ObjectKey)
		if err == nil && tag.RowsAffected() != 1 {
			return corestore.ErrRevisionConflict
		}
		return err
	})
}

func (r *Repository) AdvanceGitHubFanout(ctx context.Context, p *GitHubFanoutPage, failed bool, now time.Time) error {
	if p == nil || p.order <= 0 || now.IsZero() {
		return ErrInvalid
	}
	state := "routing"
	afterOrg, afterProject := p.afterOrganization, p.afterProject
	needsRetry := p.passFailed || failed
	attempts := p.attempts
	next := now
	if p.Staging {
		if failed {
			state = "staging"
		} else {
			attempts = 0
		}
	} else if p.Cleanup {
		state = "completed"
		if failed {
			state = "cleanup"
		}
	} else if p.exhausted {
		afterOrg, afterProject = "", ""
		if !needsRetry {
			state = "cleanup"
			attempts = 0
		}
	}
	if (p.Staging || p.Cleanup) && failed || p.exhausted && needsRetry {
		attempts++
		maximum := time.Minute
		if p.Staging {
			maximum = time.Hour
		}
		next = now.Add(githuboutbound.RetryDelay(attempts, 2*time.Second, maximum, rand.Float64()))
		needsRetry = false
	}
	return r.transaction(ctx, corestore.Background, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE platform.github_webhook_ingress SET state=$3,
		after_organization=$4,after_project=$5,pass_failed=$6,attempts=$7,next_attempt_at=$8,dispatch_order=DEFAULT,
		payload_object_key=CASE WHEN $3='completed' THEN '' ELSE payload_object_key END
		WHERE delivery_id=$1 AND dispatch_order=$2 AND state IN ('staging','routing','cleanup')`,
			p.DeliveryID, p.order, state, afterOrg, afterProject, needsRetry, attempts, next)
		if err == nil && tag.RowsAffected() != 1 {
			return corestore.ErrRevisionConflict
		}
		return err
	})
}
