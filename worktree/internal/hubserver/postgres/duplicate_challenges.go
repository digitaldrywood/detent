package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errInvalidDuplicateChallenge = errors.New("invalid duplicate challenge")

func validDuplicateIdentity(challenge corestore.DuplicateChallenge) bool {
	if challenge.OrganizationID == "" || challenge.ProjectID == "" || len(challenge.Token) != 36 || !strings.HasPrefix(challenge.Token, "dup_") || len(challenge.PrincipalID) == 0 || len(challenge.PrincipalID) > 256 || len(challenge.RequestID) == 0 || len(challenge.RequestID) > 128 || len(challenge.ContentHash) != 64 {
		return false
	}
	_, tokenErr := hex.DecodeString(challenge.Token[4:])
	_, hashErr := hex.DecodeString(challenge.ContentHash)
	return tokenErr == nil && hashErr == nil && utf8.ValidString(challenge.PrincipalID) && utf8.ValidString(challenge.RequestID)
}

func validDuplicateChallenge(challenge corestore.DuplicateChallenge) bool {
	if !validDuplicateIdentity(challenge) || len(challenge.PolicyHash) != 64 || challenge.SourceSequence < 0 || challenge.CreatedAt.IsZero() || !challenge.ExpiresAt.Equal(challenge.CreatedAt.Add(time.Hour)) || len(challenge.Candidates) == 0 || len(challenge.Candidates) > 5 {
		return false
	}
	ids := make(map[tracker.NativeWorkItemID]bool, len(challenge.Candidates))
	numbers := make(map[int64]bool, len(challenge.Candidates))
	for _, candidate := range challenge.Candidates {
		if candidate.WorkItemID == "" || len(candidate.WorkItemID) > 256 || !utf8.ValidString(string(candidate.WorkItemID)) || candidate.Number < 1 || candidate.Revision < 1 || ids[candidate.WorkItemID] || numbers[candidate.Number] {
			return false
		}
		ids[candidate.WorkItemID], numbers[candidate.Number] = true, true
	}
	return true
}

func duplicateChallengeRecord(row pgquery.NativeDuplicateChallenge) (corestore.DuplicateChallenge, error) {
	challenge := corestore.DuplicateChallenge{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, Token: row.Token, PrincipalID: row.PrincipalID, RequestID: row.RequestID, ContentHash: row.ContentHash, PolicyHash: row.PolicyHash, SourceSequence: row.SourceSequence, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt}
	if len(row.CandidateIds) != len(row.CandidateNumbers) || len(row.CandidateIds) != len(row.CandidateRevisions) {
		return challenge, errInvalidDuplicateChallenge
	}
	for i, id := range row.CandidateIds {
		challenge.Candidates = append(challenge.Candidates, corestore.DuplicateCandidate{WorkItemID: tracker.NativeWorkItemID(id), Number: row.CandidateNumbers[i], Revision: row.CandidateRevisions[i]})
	}
	if !validDuplicateChallenge(challenge) {
		return challenge, errInvalidDuplicateChallenge
	}
	return challenge, nil
}

func (s *session) DuplicateChallenge(ctx context.Context, project tracker.ProjectID, token string) (corestore.DuplicateChallenge, error) {
	row, err := s.queries.DuplicateChallenge(ctx, pgquery.DuplicateChallengeParams{OrganizationID: s.organization, ProjectID: project, Token: token})
	if err != nil {
		return corestore.DuplicateChallenge{}, mapError(ctx, err)
	}
	return duplicateChallengeRecord(row)
}

func (s *session) CreateDuplicateChallenge(ctx context.Context, challenge corestore.DuplicateChallenge) error {
	if challenge.OrganizationID != s.organization || !validDuplicateChallenge(challenge) {
		return errInvalidDuplicateChallenge
	}
	params := pgquery.CreateDuplicateChallengeParams{OrganizationID: s.organization, ProjectID: challenge.ProjectID, Token: challenge.Token, PrincipalID: challenge.PrincipalID, RequestID: challenge.RequestID, ContentHash: challenge.ContentHash, PolicyHash: challenge.PolicyHash, SourceSequence: challenge.SourceSequence, CreatedAt: challenge.CreatedAt.UTC(), ExpiresAt: challenge.ExpiresAt.UTC()}
	for _, candidate := range challenge.Candidates {
		params.CandidateIds = append(params.CandidateIds, string(candidate.WorkItemID))
		params.CandidateNumbers = append(params.CandidateNumbers, candidate.Number)
		params.CandidateRevisions = append(params.CandidateRevisions, candidate.Revision)
	}
	return mapError(ctx, s.queries.CreateDuplicateChallenge(ctx, params))
}

func (s *session) ConsumeDuplicateChallenge(ctx context.Context, challenge corestore.DuplicateChallenge, now time.Time) (corestore.DuplicateChallenge, error) {
	if challenge.OrganizationID != s.organization || !validDuplicateIdentity(challenge) || now.IsZero() {
		return corestore.DuplicateChallenge{}, errInvalidDuplicateChallenge
	}
	row, err := s.queries.ConsumeDuplicateChallenge(ctx, pgquery.ConsumeDuplicateChallengeParams{OrganizationID: s.organization, ProjectID: challenge.ProjectID, Token: challenge.Token, PrincipalID: challenge.PrincipalID, RequestID: challenge.RequestID, ContentHash: challenge.ContentHash, PolicyHash: challenge.PolicyHash, SourceSequence: challenge.SourceSequence, Now: now.UTC()})
	if err != nil {
		return corestore.DuplicateChallenge{}, mapError(ctx, err)
	}
	return duplicateChallengeRecord(row)
}

func (s *session) PruneDuplicateChallenges(ctx context.Context, now time.Time, limit int) (int64, error) {
	if now.IsZero() || limit < 1 || limit > corestore.MaxDuplicateChallengePruneBatch {
		return 0, corestore.ErrInvalidPage
	}
	count, err := s.queries.PruneDuplicateChallenges(ctx, pgquery.PruneDuplicateChallengesParams{OrganizationID: string(s.organization), Now: now.UTC(), BatchLimit: int32(limit)})
	return count, mapError(ctx, err)
}
