package serviceapi

const (
	AddressEnvironment = "DETENT_SERVICE_ADDRESS"
	TokenEnvironment   = "DETENT_API_TOKEN"
)

// Connection identifies the running Detent service available to a worker.
type Connection struct {
	Address string
	Token   string
}

// Environment returns explicit worker process overrides for the connection.
func (c Connection) Environment() map[string]string {
	if c.Address == "" && c.Token == "" {
		return nil
	}
	return map[string]string{
		AddressEnvironment: c.Address,
		TokenEnvironment:   c.Token,
	}
}
