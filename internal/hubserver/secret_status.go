package hubserver

type SecretVersionCount struct {
	Table            string `json:"table"`
	Kind             string `json:"kind"`
	MasterKeyVersion int    `json:"master_key_version"`
	Count            int    `json:"count"`
}

type SecretRowProblem struct {
	Database         string `json:"database"`
	OrganizationID   string `json:"organization_id"`
	ProjectID        string `json:"project_id"`
	Kind             string `json:"kind"`
	MasterKeyVersion int    `json:"master_key_version"`
	UpdatedAt        string `json:"updated_at"`
	Problem          string `json:"problem"`
}

type SecretDatabaseStatus struct {
	Database string               `json:"database"`
	Counts   []SecretVersionCount `json:"counts"`
	Problems []SecretRowProblem   `json:"problems"`
	Problem  string               `json:"problem,omitempty"`
}
