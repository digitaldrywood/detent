package tenant

import "time"

type Organization struct {
	HostedConfigurationKey *string   `json:"-"`
	WorkspacePolicyKey     *string   `json:"-"`
	ID                     string    `json:"id"`
	ProviderID             string    `json:"provider_organization_id"`
	Slug                   string    `json:"slug"`
	Name                   string    `json:"name"`
	State                  string    `json:"state"`
	Endpoint               string    `json:"endpoint"`
	Generation             int64     `json:"generation"`
	Managed                bool      `json:"managed"`
	CheckoutPrice          string    `json:"-"`
	CreatorSubject         string    `json:"-"`
	CreatorEmail           string    `json:"-"`
	Step                   string    `json:"step,omitempty"`
	Attempts               int       `json:"attempts,omitempty"`
	NextAttemptAt          string    `json:"-"`
	ErrorCode              string    `json:"error_code,omitempty"`
	ErrorDetail            string    `json:"error_detail,omitempty"`
	UpdatedAt              time.Time `json:"updated_at"`
}
