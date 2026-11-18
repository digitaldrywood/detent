package cli

const clihangCloudPostgres = `storage:
  backend: postgres
  postgres_dsn_env: CLIHANG_PLATFORM_DSN
tenant_postgres:
  host: db.example.test
  port: 5432
  database: example
  user: example_runtime
  password_env: CLIHANG_TENANT_PASSWORD
  sslmode: verify-full
  max_connections: 4
  serving_connections: 2
  background_connections: 1
`

const clihangCloudAttachments = `attachments:
  endpoint: https://objects.example.test
  region: example
  bucket: example-private
`

func clihangCloudLookup(env map[string]string) func(string) string {
	return func(name string) string {
		if value := env[name]; value != "" {
			return value
		}
		switch name {
		case "CLIHANG_PLATFORM_DSN":
			return "postgres://example:fixture@127.0.0.1:1/example?sslmode=disable"
		case "CLIHANG_TENANT_PASSWORD":
			return "example-password"
		case "DETENT_ATTACHMENTS_ACCESS_KEY_ID":
			return "example-access-key"
		case "DETENT_ATTACHMENTS_SECRET_ACCESS_KEY":
			return "example-secret-key"
		default:
			return env[name]
		}
	}
}
