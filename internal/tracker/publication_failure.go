package tracker

type PublicationFailure struct {
	RefusalCode  string `json:"refusal_code,omitempty"`
	Operation    string `json:"operation"`
	Code         string `json:"code"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	RequestBytes int64  `json:"request_bytes"`
	BodyBytes    int64  `json:"body_bytes"`
	OutputBytes  int64  `json:"output_bytes"`
}

func (f PublicationFailure) Valid() bool {
	if f.Operation != "security_audit/review" || f.RequestBytes < 0 || f.BodyBytes < 0 || f.OutputBytes < 0 {
		return false
	}
	switch f.RefusalCode {
	case "", "invalid_request", "payload_too_large":
	case "revision_conflict", "conflict", "policy_mismatch", "stale_fencing_token", "run_sequence_conflict", "idempotency_conflict", "idempotency_in_progress":
	case "forbidden", "not_found", "unauthorized", "access_denied", "insufficient_scope", "native_protocol_required":
	case "service_unavailable", "unavailable", "not_ready", "storage_unreachable", "storage_owner_unavailable", "native_operation_failed", "allowance_exhausted":
	default:
		return false
	}
	switch f.Code {
	case "http_rejection":
		return f.HTTPStatus >= 300 && f.HTTPStatus <= 599
	case "request_failed":
		return f.HTTPStatus == 0 && f.RefusalCode == ""
	default:
		return false
	}
}
