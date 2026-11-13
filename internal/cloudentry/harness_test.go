package cloudentry

import "github.com/digitaldrywood/detent/internal/cloudentry/internal/harness"

type (
	fakeProvider               = harness.FakeProvider
	browser                    = harness.Browser
	page                       = harness.Page
	spacesFixture              = harness.Spaces
	spacesObject               = harness.SpacesObject
	conversationArchiveFixture = harness.ConversationArchive
	tenantFixture              = harness.Tenant
	tenantDetailFixture        = harness.TenantDetails
)

const (
	testPublicURL             = harness.PublicURL
	testAdminKey              = harness.AdminKey
	issueContractTestSections = harness.IssueContractSections
)

var (
	entryPostgresTemplate   = harness.Postgres
	newFakeProvider         = harness.NewFakeProvider
	newBrowser              = harness.NewBrowser
	csrfFrom                = harness.CSRF
	bootstrapCSRF           = harness.BootstrapCSRF
	jsonBody                = harness.JSONBody
	newSpacesFixture        = harness.NewSpaces
	newTenant               = harness.NewTenant
	pilotPlans              = harness.PilotPlans
	capacityPlans           = harness.CapacityPlans
	testSocketEndpoint      = harness.SocketEndpoint
	signedTenantFixtureCall = harness.SignedTenantCall
	pilotPolicy             = harness.PilotPolicy
	entryPostgresDatabase   = harness.PostgresDatabase
	configurePostgresTenant = harness.ConfigurePostgresTenant
	entrySecretEnvironment  = harness.SecretEnvironment
)
