package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type preparedLandingFact struct {
	change, version, frozen, project string
	changeID, head, landed           string
	github                           bool
	id                               string
}

func resolveLandingProject(ctx context.Context, q nativeQueryer, project string, github bool) (string, error) {
	if !strings.HasPrefix(project, "?") {
		return project, nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(project, "?"), 10, 64)
	if err != nil {
		return "", err
	}
	var fact preparedLandingFact
	var changeID, versionID string
	if err := q.QueryRowContext(ctx, `SELECT i.project_id,c.id,c.record_json FROM issues i JOIN change_requests c ON c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id WHERE i.id=? ORDER BY c.rowid DESC LIMIT 1`, id).Scan(&fact.project, &changeID, &fact.change); err != nil {
		return "", err
	}
	prepared := claimPrepared(ctx)
	if prepared != nil {
		if cached, ok := prepared.landings[project]; ok && cached.change == fact.change {
			var versionRaw string
			err := q.QueryRowContext(ctx, `SELECT record_json FROM change_versions WHERE change_id=? AND id=?`, changeID, cached.id).Scan(&versionRaw)
			if err != nil {
				return "", err
			}
			var frozen sql.NullString
			if err := q.QueryRowContext(ctx, `SELECT (SELECT metadata_json FROM policy_revisions frozen WHERE frozen.scope=i.organization_id||'/'||i.project_id AND frozen.policy_id=v.policy_revision_id) FROM issues i JOIN change_versions v ON v.id=? WHERE i.id=?`, cached.id, id).Scan(&frozen); err != nil {
				return "", err
			}
			if cached.version == versionRaw && cached.frozen == frozen.String {
				if cached.github == github {
					return cached.project, nil
				}
				return "", nil
			}
		}
		if claimWriting(ctx) {
			return "", errClaimHintStale
		}
		if !prepared.hydrating {
			return "", &claimHydration{run: func(ctx context.Context, q nativeQueryer) error {
				_, err := resolveLandingProject(ctx, q, project, github)
				return err
			}}
		}
	}
	var change tracker.ChangeRequest
	if err := json.Unmarshal([]byte(fact.change), &change); err != nil {
		return "", err
	}
	fact.changeID = changeID
	if change.Landed != nil {
		fact.landed = change.Landed.VersionID
	}
	versionID = change.CurrentVersion
	fact.id = versionID
	if versionID == "" {
		return "", nil
	}
	var policyID string
	if err := q.QueryRowContext(ctx, `SELECT record_json,policy_revision_id FROM change_versions WHERE change_id=? AND id=?`, changeID, versionID).Scan(&fact.version, &policyID); err != nil {
		return "", err
	}
	err = q.QueryRowContext(ctx, `SELECT metadata_json FROM policy_revisions frozen JOIN issues i ON frozen.scope=i.organization_id||'/'||i.project_id WHERE i.id=? AND frozen.policy_id=?`, id, policyID).Scan(&fact.frozen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	fact.head, fact.github, err = decodeLandingVersionFacts(ctx, q, fact.version, fact.frozen)
	if err != nil {
		return "", err
	}
	if prepared != nil {
		if prepared.landings == nil {
			prepared.landings = make(map[string]preparedLandingFact)
		}
		prepared.landings[project] = fact
	}
	if fact.github == github {
		return fact.project, nil
	}
	return "", nil
}

func decodeLandingVersionFacts(ctx context.Context, q nativeQueryer, raw, frozen string) (string, bool, error) {
	var version struct {
		HeadSHA string             `json:"head_sha"`
		Policy  *policy.Descriptor `json:"policy"`
	}
	if frozen != "" {
		if err := json.Unmarshal([]byte(raw), &version); err != nil {
			return "", false, err
		}
		var descriptor policy.Descriptor
		if err := json.Unmarshal([]byte(frozen), &descriptor); err != nil {
			return "", false, err
		}
		return version.HeadSHA, descriptor.Gates.GitHubPullRequest, nil
	}
	if err := json.Unmarshal([]byte(raw), &version); err != nil {
		return "", false, err
	}
	if version.Policy == nil {
		if _, transaction := q.(*sql.Tx); transaction {
			return "", false, errClaimHintStale
		}
		if err := decodeStorageJSON(ctx, []byte(raw), &version); err != nil {
			return "", false, err
		}
	}
	if version.Policy == nil {
		return "", false, fmt.Errorf("read version landing policy: %w", errClaimHintStale)
	}
	return version.HeadSHA, version.Policy.Gates.GitHubPullRequest, nil
}
