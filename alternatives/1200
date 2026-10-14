package linear

import (
	"context"
	"fmt"
	"strings"
)

const usersByLoginQuery = `
query DetentLinearUserByLogin($filter: UserFilter!) {
  users(filter: $filter, first: 2) {
    nodes {
      id
      name
      displayName
      email
    }
  }
}`

func (c *Connector) resolveUserID(ctx context.Context, login string) (string, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return "", ErrMissingUser
	}

	loginKey := normalizeUserLogin(login)
	if userID, ok := c.cachedUserID(loginKey); ok {
		return userID, nil
	}

	userID, err := c.resolveUncachedUserID(ctx, login)
	if err != nil {
		return "", err
	}

	c.cacheUserID(loginKey, userID)
	return userID, nil
}

func (c *Connector) cachedUserID(loginKey string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	userID := c.userIDByLogin[loginKey]
	return userID, userID != ""
}

func (c *Connector) cacheUserID(loginKey string, userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.userIDByLogin == nil {
		c.userIDByLogin = make(map[string]string)
	}
	c.userIDByLogin[loginKey] = userID
}

func (c *Connector) resolveUncachedUserID(ctx context.Context, login string) (string, error) {
	for _, lookup := range linearUserLookups(login) {
		users, err := c.fetchUsersByFilter(ctx, lookup.filter)
		if err != nil {
			return "", fmt.Errorf("resolve linear user: %w", err)
		}
		userID, ok, err := selectLinearUserID(login, users, lookup.matches)
		if err != nil {
			return "", err
		}
		if ok {
			return userID, nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrUserNotFound, login)
}

func (c *Connector) fetchUsersByFilter(ctx context.Context, filter map[string]any) ([]linearResolvedUser, error) {
	var response struct {
		Users struct {
			Nodes []linearResolvedUser `json:"nodes"`
		} `json:"users"`
	}
	if err := c.client.GraphQL(ctx, usersByLoginQuery, map[string]any{
		"filter": filter,
	}, &response); err != nil {
		return nil, err
	}
	return response.Users.Nodes, nil
}

func linearUserLookups(login string) []linearUserLookup {
	return []linearUserLookup{
		{
			filter: linearUserEmailFilter(login),
			matches: func(user linearResolvedUser) bool {
				return strings.TrimSpace(user.Email) == login
			},
		},
		{
			filter: linearUserDisplayNameFilter(login),
			matches: func(user linearResolvedUser) bool {
				return strings.EqualFold(strings.TrimSpace(user.DisplayName), login)
			},
		},
		{
			filter: linearUserNameFilter(login),
			matches: func(user linearResolvedUser) bool {
				return strings.EqualFold(strings.TrimSpace(user.Name), login)
			},
		},
	}
}

func linearUserEmailFilter(login string) map[string]any {
	return map[string]any{"email": map[string]any{"eq": login}}
}

func linearUserDisplayNameFilter(login string) map[string]any {
	return map[string]any{"displayName": map[string]any{"eqIgnoreCase": login}}
}

func linearUserNameFilter(login string) map[string]any {
	return map[string]any{"name": map[string]any{"eqIgnoreCase": login}}
}

func selectLinearUserID(login string, users []linearResolvedUser, matches func(linearResolvedUser) bool) (string, bool, error) {
	userIDs, err := matchingUserIDs(users, matches)
	if err != nil {
		return "", false, err
	}
	switch len(userIDs) {
	case 0:
		return "", false, nil
	case 1:
		return userIDs[0], true, nil
	default:
		return "", false, fmt.Errorf("%w: %s", ErrUserAmbiguous, login)
	}
}

func matchingUserIDs(users []linearResolvedUser, matches func(linearResolvedUser) bool) ([]string, error) {
	seen := map[string]struct{}{}
	userIDs := []string{}
	for _, user := range users {
		if !matches(user) {
			continue
		}
		userID := strings.TrimSpace(user.ID)
		if userID == "" {
			return nil, ErrInvalidResponse
		}
		if _, ok := seen[userID]; ok {
			continue
		}
		seen[userID] = struct{}{}
		userIDs = append(userIDs, userID)
	}
	return userIDs, nil
}

func normalizeUserLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

type linearResolvedUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

type linearUserLookup struct {
	filter  map[string]any
	matches func(linearResolvedUser) bool
}
