package accessdirectory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/platformusers"
)

func (s *Service) ServeHTTP(c echo.Context) error {
	if claims, ok := s.deps.Claims(c); !ok || claims.Kind != cloudassert.KindService {
		return s.deps.Error(c, hubapi.NotFound())
	}
	var input platformusers.Query
	if err := hubapi.DecodeJSON(c, &input); err != nil {
		return s.deps.Invalid(c, err)
	}
	query := platformusers.Parse(input.Text)
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	result := platformusers.TenantResult{Members: []platformusers.TenantMember{}, Invitations: []platformusers.TenantInvitation{}}

	return s.Search(c, ctx, query, input, result)
}

type candidate struct {
	cursor, email, subject, role, at string
}

func (s *Service) Search(c echo.Context, ctx context.Context, query platformusers.Query, input platformusers.Query, result platformusers.TenantResult) error {
	candidates := make([]candidate, 0, 2*(platformusers.Limit+1))
	memberAfter, memberLimit := platformUserCursorRange(input.After, "m:")
	invitationAfter, invitationLimit := platformUserCursorRange(input.After, "i:")
	match := func(email string) bool {
		email = strings.ToLower(email)
		if query.Exact {
			return email == query.Text
		}
		return strings.Contains(email, query.Text)
	}
	err := s.read(ctx, func(r tenantstore.Reader) error {
		if memberLimit > 0 {
			after := memberAfter
			for lenMemberMatches(candidates) < memberLimit {
				page, err := r.AccessMembers(ctx, after, 200)
				if err != nil {
					return err
				}
				for _, member := range page {
					if member.Active && match(member.Email) {
						cursor := "m:" + member.UserID
						if cursor > input.After {
							candidates = append(candidates, candidate{cursor: cursor, email: member.Email, subject: member.UserID, role: member.Role, at: member.CreatedAt.UTC().Format(time.RFC3339Nano)})
						}
					}
				}
				if len(page) < 200 {
					break
				}
				after = page[len(page)-1].UserID
			}
		}
		if invitationLimit > 0 {
			after := invitationAfter
			for lenInvitationMatches(candidates) < invitationLimit {
				page, err := r.AccessInvitations(ctx, after, 200)
				if err != nil {
					return err
				}
				for _, invitation := range page {
					if invitation.AcceptedUserID == "" && invitation.ExpiresAt.After(s.deps.Now()) && match(invitation.Email) {
						cursor := "i:" + invitation.ID
						if cursor > input.After {
							candidates = append(candidates, candidate{cursor: cursor, email: invitation.Email, at: invitation.ExpiresAt.UTC().Format(time.RFC3339Nano)})
						}
					}
				}
				if len(page) < 200 {
					break
				}
				after = page[len(page)-1].ID
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].cursor < candidates[j].cursor })
	processed := 0
	more := len(candidates) > platformusers.Limit
	for _, candidate := range candidates {
		if processed == platformusers.Limit {
			more = true
			break
		}
		previous := result
		if strings.HasPrefix(candidate.cursor, "m:") {
			result.Members = append(result.Members, platformusers.TenantMember{Email: candidate.email, Subject: candidate.subject, Role: candidate.role, JoinedAt: candidate.at})
		} else {
			result.Invitations = append(result.Invitations, platformusers.TenantInvitation{Email: candidate.email, ExpiresAt: candidate.at})
		}
		result.NextCursor = candidate.cursor
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if len(body) > 60<<10 {
			if processed == 0 {
				return errors.New("platform user exceeds tenant page limit")
			}
			result = previous
			more = true
			break
		}
		processed++
	}
	if !more {
		result.NextCursor = ""
	}
	return c.JSON(http.StatusOK, result)
}

func lenMemberMatches(candidates []candidate) int {
	count := 0
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate.cursor, "m:") {
			count++
		}
	}
	return count
}

func lenInvitationMatches(candidates []candidate) int {
	count := 0
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate.cursor, "i:") {
			count++
		}
	}
	return count
}

func platformUserCursorRange(after, prefix string) (string, int) {
	if strings.HasPrefix(after, prefix) {
		return after[len(prefix):], platformusers.Limit + 1
	}
	if after < prefix {
		return "", platformusers.Limit + 1
	}
	return "", 0
}

type Deps struct {
	Repository   tenantstore.Repository
	Organization tracker.OrganizationID
	Now          func() time.Time
	Claims       func(echo.Context) (cloudassert.Claims, bool)
	Error        func(echo.Context, error) error
	Invalid      func(echo.Context, error) error
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
func (s *Service) read(ctx context.Context, read func(tenantstore.Reader) error) error {
	if s.deps.Repository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.Repository.ReadTenant(ctx, s.deps.Organization, corestore.Serving, read)
}
