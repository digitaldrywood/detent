package tracker

type PublicationFailure struct {
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
	switch f.Code {
	case "http_rejection":
		return f.HTTPStatus >= 300 && f.HTTPStatus <= 599
	case "request_failed":
		return f.HTTPStatus == 0
	default:
		return false
	}
}
