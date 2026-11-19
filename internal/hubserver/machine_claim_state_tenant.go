package hubserver

import (
	"errors"
)

func (s *Service) runnerCleanupCursor(key releasedExecutionCursorKey) (string, error) {
	if position, found := s.cleanupPollCursors.Load(key); found {
		after, ok := position.(string)
		if !ok {
			return "", errors.New("runner cleanup cursor has invalid type")
		}
		return after, nil
	}
	return "", nil
}

func (s *Service) advanceRunnerCleanupCursor(key releasedExecutionCursorKey, next string) {
	if next == "" {
		s.cleanupPollCursors.Delete(key)
	} else {
		s.cleanupPollCursors.Store(key, next)
	}
}
