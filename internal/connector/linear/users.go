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

	users, err := c.fetchUsersByLogin(ctx, login)
	if err != nil {
		return "", fmt.Errorf("resolve linear user: %w", err)
	}
	userID, err := selectLinearUserID(login, users)
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

func (c *Connector) fetchUsersByLogin(ctx context.Context, login string) ([]linearResolvedUser, error) {
	var response struct {
		Users struct {
			Nodes []linearResolvedUser `json:"nodes"`
		} `json:"users"`
	}
	if err := c.client.GraphQL(ctx, usersByLoginQuery, map[string]any{
		"filter": linearUserLoginFilter(login),
	}, &response); err != nil {
		return nil, err
	}
	return response.Users.Nodes, nil
}

func linearUserLoginFilter(login string) map[string]any {
	return map[string]any{
		"or": []map[string]any{
			{"email": map[string]any{"eq": login}},
			{"displayName": map[string]any{"eqIgnoreCase": login}},
			{"name": map[string]any{"eqIgnoreCase": login}},
		},
	}
}

func selectLinearUserID(login string, users []linearResolvedUser) (string, error) {
	matchers := []func(linearResolvedUser) bool{
		func(user linearResolvedUser) bool {
			return strings.TrimSpace(user.Email) == login
		},
		func(user linearResolvedUser) bool {
			return strings.EqualFold(strings.TrimSpace(user.DisplayName), login)
		},
		func(user linearResolvedUser) bool {
			return strings.EqualFold(strings.TrimSpace(user.Name), login)
		},
	}

	for _, matches := range matchers {
		userIDs, err := matchingUserIDs(users, matches)
		if err != nil {
			return "", err
		}
		switch len(userIDs) {
		case 0:
			continue
		case 1:
			return userIDs[0], nil
		default:
			return "", fmt.Errorf("%w: %s", ErrUserAmbiguous, login)
		}
	}

	return "", fmt.Errorf("%w: %s", ErrUserNotFound, login)
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
