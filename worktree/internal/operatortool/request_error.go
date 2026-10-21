package operatortool

type RequestError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RequestError) Error() string { return e.Code }

func (e *RequestError) Unwrap() error { return ErrInvalidArguments }
