package hubserver

var analyticsProjectionFields = []struct{ column, key string }{
	{"work_item_id", "work_item_id"}, {"sequence", "sequence"}, {"type", "type"},
	{"recorded_at", "recorded_at"}, {"started_at", "started_at"}, {"updated_at", "updated_at"},
	{"attempt_id", "attempt_id"}, {"period", "period"}, {"sample_id", "ID"},
	{"sample_at", "At"}, {"sample_ns", "AtNS"}, {"sample_start_ns", "StartNS"}, {"sample_from_ns", "FromNS"},
	{"skip_source", "Source"}, {"skip_reason", "Reason"}, {"last_at", "last_at"}, {"last_ns", "last_ns"},
	{"issue_id", "id"}, {"native_id", "native_id"}, {"title", "title"}, {"created_at", "created_at"},
	{"workflow_state_id", "workflow_state_id"}, {"lease_id", "lease_id"}, {"status", "status"},
	{"provider", "provider"}, {"model", "model"}, {"input", "input"}, {"cached_input", "cached_input"},
	{"output", "output"}, {"currency", "currency"}, {"acquired_at", "acquired_at"},
	{"released_at", "released_at"}, {"expires_at", "expires_at"}, {"failed_at", "failed_at"},
	{"scheduler_outcome", "outcome"}, {"scheduler_source", "source"}, {"scheduler_reason", "reason"},
	{"analytics_to_state", "to_state"},
}
