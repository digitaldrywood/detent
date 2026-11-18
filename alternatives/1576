package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const boardAccessCacheLimit = 4096

type boardAccessEntry struct {
	allowed   []tracker.ProjectID
	revisions [4]int64
}

type boardAccessCache struct {
	mu      sync.Mutex
	entries map[string]boardAccessEntry
	order   []string
}

func boardAccessKey(scope hubscope.NativeScope) string {
	credential := scope.Credential
	subject := ""
	if credential.Hosted != nil {
		subject = credential.Hosted.Subject
	}
	raw, err := json.Marshal([]any{scope.Organization, credential.ID, credential.HostedPrincipal, subject, credential.HostedMembership, credential.HostedRole,
		credential.HostedKeyScope, credential.HostedProjectAccess, credential.HostedKeyKind, credential.Scope, credential.NativeOnly, credential.EntryKey})
	if err != nil {
		return ""
	}
	return string(raw)
}

func (c *boardAccessCache) get(key string) (boardAccessEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, found := c.entries[key]
	return entry, found
}

func (c *boardAccessCache) put(key string, entry boardAccessEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]boardAccessEntry{}
	}
	if _, found := c.entries[key]; !found {
		c.order = append(c.order, key)
	}
	c.entries[key] = entry
	for len(c.order) > boardAccessCacheLimit {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
}

func (c *boardAccessCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (s *Service) boardAllowedProjects(ctx context.Context, scope hubscope.NativeScope) (boardAccessEntry, error) {
	key := boardAccessKey(scope)
	if key == "" {
		return boardAccessEntry{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	if entry, found := s.boardAccess.get(key); found {
		return entry, nil
	}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return boardAccessEntry{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	var entry boardAccessEntry
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		if scope.Credential.Hosted == nil {
			entry.allowed, err = s.boardKeyReach(ctx, reader, scope)
		} else {
			entry.allowed, err = s.boardMemberReach(ctx, reader, scope)
		}
		if err != nil {
			return err
		}
		if key := scope.Credential.EntryKey; key != nil {
			entry.allowed = slices.DeleteFunc(entry.allowed, func(project tracker.ProjectID) bool { return !key.Allows(string(project)) })
		}
		return s.boardAccessRevisions(ctx, reader, scope, &entry)
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return boardAccessEntry{}, hubapi.NativeNotFound()
	}
	if err != nil {
		return boardAccessEntry{}, err
	}
	s.boardAccess.put(key, entry)
	return entry, nil
}

func (s *Service) boardKeyReach(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope) ([]tracker.ProjectID, error) {
	lister, ok := reader.(interface {
		Projects(context.Context, tracker.ProjectID, int) ([]corestore.Project, error)
	})
	if !ok {
		return nil, hubapi.ErrProjectParticipants
	}
	const pageSize = 200
	allowed := []tracker.ProjectID{}
	after := tracker.ProjectID("")
	for {
		page, err := lister.Projects(ctx, after, pageSize)
		if err != nil {
			return nil, err
		}
		for _, project := range page {
			if project.DeletedAt != nil || project.Profile != "native" {
				continue
			}
			projectScope := scope
			projectScope.Project = project.ID
			err := s.authorizeAccessScope(ctx, reader, projectScope, false)
			if errors.Is(err, accessstore.ErrDenied) || hubapi.IsNativeNotFound(err) || errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			allowed = append(allowed, project.ID)
		}
		if len(page) < pageSize {
			return allowed, nil
		}
		after = page[len(page)-1].ID
	}
}

func (s *Service) boardMemberReach(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope) ([]tracker.ProjectID, error) {
	pages, ok := reader.(accessstore.ProjectAccessPageReader)
	if !ok {
		return nil, hubapi.ErrProjectParticipants
	}
	const pageSize = 200
	var rows []accessstore.ProjectAccess
	after := tracker.ProjectID("")
	for {
		page, err := pages.ProjectAccessPage(ctx, projectAccessFilter(scope.Credential, "", after, pageSize))
		if err != nil {
			return nil, err
		}
		if len(page) > pageSize {
			rows = append(rows, page[:pageSize]...)
			after = page[pageSize-1].ProjectID
			continue
		}
		rows = append(rows, page...)
		break
	}
	granted, err := s.grantedAccessProjects(ctx, reader, scope.Organization, scope.Credential, rows)
	if err != nil {
		return nil, err
	}
	allowed := make([]tracker.ProjectID, 0, len(granted))
	for _, row := range granted {
		allowed = append(allowed, row.ProjectID)
	}
	return allowed, nil
}

func (s *Service) boardAccessRevisions(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, entry *boardAccessEntry) error {
	authority := s.accessAuthority(scope, false)
	credential, err := reader.BoardAccessRevisions(ctx, authority.CredentialID, authority.UserID)
	if err != nil {
		return err
	}
	principal, err := reader.BoardAccessRevisions(ctx, scope.Credential.HostedPrincipal, "")
	if err != nil {
		return err
	}
	entry.revisions = [4]int64{credential[0], credential[1], principal[1], credential[2]}
	return nil
}
