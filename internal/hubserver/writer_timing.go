package hubserver

import "time"

func (s *Service) logWriterWaits() {
	if s.database == nil || s.database.postgres == nil {
		return
	}
	now := time.Now().UnixNano()
	last := s.writerStatsAt.Load()
	if now-last < int64(time.Minute) || !s.writerStatsAt.CompareAndSwap(last, now) {
		return
	}

	stats := s.database.postgres.Stats()
	waits, waited := stats.EmptyAcquireCount()-s.writerWaits.Swap(stats.EmptyAcquireCount()), stats.EmptyAcquireWaitTime()-time.Duration(s.writerWaited.Swap(int64(stats.EmptyAcquireWaitTime())))
	if last == 0 {
		return
	}
	serving, background := s.database.postgres.WorkloadCapacities()
	s.config.Logger.Info("hub PostgreSQL pool waits", "waits", waits, "wait_ms", waited.Milliseconds(), "in_use", stats.AcquiredConns(), "max_open", stats.MaxConns(), "serving_capacity", serving, "background_capacity", background, "interval_s", (now-last)/int64(time.Second))
}
