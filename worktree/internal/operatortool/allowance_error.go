package operatortool

type AllowanceError struct {
	Code        string `json:"code"`
	HTTPStatus  int    `json:"http_status"`
	Message     string `json:"message"`
	Resource    string `json:"resource"`
	Allowance   int64  `json:"allowance"`
	Consumption int64  `json:"consumption"`
	Recovery    string `json:"recovery"`
}

func (e *AllowanceError) Error() string { return e.Code }
