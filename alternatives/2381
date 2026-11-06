package hubserver

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) resolveOperatorRecordReadItem(ctx context.Context, scope nativeScope, reference string) (tracker.NativeIssue, error) {
	if strings.Contains(reference, "/") {
		id, err := s.readableReference(ctx, nil, scope, reference)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		reference = id
	}
	nativeID := strings.HasPrefix(reference, "wi_")
	number := int64(0)
	if !nativeID {
		value := reference
		if prefix, suffix, found := strings.Cut(reference, "#"); found {
			if prefix != "" && prefix != string(scope.project) {
				return tracker.NativeIssue{}, explain.ErrNotFound
			}
			value = suffix
		}
		var err error
		number, err = strconv.ParseInt(value, 10, 64)
		if err != nil || number < 1 {
			return tracker.NativeIssue{}, explain.ErrNotFound
		}
	}
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if nativeID {
			row, err = r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(reference))
			if errors.Is(err, corestore.ErrNotFound) && scope.credential.HostedRole != "reporter" {
				other, lookupErr := r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(reference))
				if lookupErr != nil {
					return lookupErr
				}
				if other.ProjectID != scope.project {
					otherScope := scope
					otherScope.project = other.ProjectID
					authErr := s.authorizeAccessScope(ctx, r, otherScope, false)
					if errors.Is(authErr, accessstore.ErrDenied) || errors.Is(authErr, auth.ErrHostedIdentity) || errors.Is(authErr, corestore.ErrNotFound) {
						return operatortool.ErrProjectScopeRequired
					}
					if authErr != nil {
						return authErr
					}
				}
				return corestore.ErrNotFound
			}
		} else {
			row, err = r.WorkItemByNumber(ctx, scope.project, number)
		}
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		version, err = r.CollaborationVersion(ctx, string(row.NativeID), row.Revision)
		return err
	})
	if err != nil {
		return tracker.NativeIssue{}, projectRecordError(err)
	}
	item, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err == nil {
		err = s.hydrateWorkItemRelationshipFacts(ctx, scope, &item)
	}
	item.OmittedFields = append(item.OmittedFields, "change_sequence")
	return scope.reporterIssue(item), err
}
