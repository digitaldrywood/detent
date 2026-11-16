package hubserver

import (
	"context"
	"encoding/json"
	"errors"
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

func boardAccessKey(scope hubscope.NativeScope, requested []tracker.ProjectID) string {
	credential := scope.Credential
	subject := ""
	if credential.Hosted != nil {
		subject = credential.Hosted.Subject
	}
	raw, err := json.Marshal([]any{scope.Organization, credential.ID, credential.HostedPrincipal, subject, credential.HostedMembership, credential.HostedRole,
		credential.HostedKeyScope, credential.HostedProjectAccess, credential.HostedKeyKind, credential.Scope, credential.NativeOnly, credential.EntryKey, boardAccessRequested(scope, requested)})
	if err != nil {
		return ""
	}
	return string(raw)
}

func boardAccessRequested(scope hubscope.NativeScope, requested []tracker.ProjectID) []tracker.ProjectID {
	if scope.Credential.Hosted != nil {
		return nil
	}
	return requested
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

func (s *Service) boardAllowedProjects(ctx context.Context, scope hubscope.NativeScope, requested []tracker.ProjectID) (boardAccessEntry, error) {
	key := boardAccessKey(scope, requested)
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
		var rows []accessstore.ProjectAccess
		if scope.Credential.Hosted == nil {
			for _, project := range requested {
				projectScope := scope
				projectScope.Project = project
				err := s.authorizeAccessScope(ctx, reader, projectScope, false)
				if errors.Is(err, accessstore.ErrDenied) || hubapi.IsNativeNotFound(err) || errors.Is(err, corestore.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				entry.allowed = append(entry.allowed, project)
			}
			return s.boardAccessRevisions(ctx, reader, scope, &entry)
		}
		pages, ok := reader.(accessstore.ProjectAccessPageReader)
		if !ok {
			return hubapi.ErrProjectParticipants
		}
		const pageSize = 200
		after := tracker.ProjectID("")
		for {
			page, err := pages.ProjectAccessPage(ctx, projectAccessFilter(scope.Credential, "", after, pageSize))
			if err != nil {
				return err
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
			return err
		}
		for _, row := range granted {
			entry.allowed = append(entry.allowed, row.ProjectID)
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
