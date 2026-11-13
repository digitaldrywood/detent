package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

func (s *Service) authorization() hubscope.Authorization {
	return hubscope.Authorization{Access: s.authorizeAccessScope, WorkItem: s.authorizeWorkItemRecordScope}
}
