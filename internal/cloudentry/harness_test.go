package cloudentry

import "github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"

type (
	fakeProvider               = entrytest.FakeProvider
	browser                    = entrytest.Browser
	page                       = entrytest.Page
	spacesFixture              = entrytest.Spaces
	spacesObject               = entrytest.SpacesObject
	conversationArchiveFixture = entrytest.ConversationArchive
	tenantFixture              = entrytest.Tenant
	tenantDetailFixture        = entrytest.TenantDetails
)

const (
	testPublicURL             = entrytest.PublicURL
	testAdminKey              = entrytest.AdminKey
	issueContractTestSections = entrytest.IssueContractSections
)

var (
	entryPostgresTemplate   = entrytest.Postgres
	newFakeProvider         = entrytest.NewFakeProvider
	newBrowser              = entrytest.NewBrowser
	csrfFrom                = entrytest.CSRF
	bootstrapCSRF           = entrytest.BootstrapCSRF
	jsonBody                = entrytest.JSONBody
	newSpacesFixture        = entrytest.NewSpaces
	newTenant               = entrytest.NewTenant
	pilotPlans              = entrytest.PilotPlans
	capacityPlans           = entrytest.CapacityPlans
	testSocketEndpoint      = entrytest.SocketEndpoint
	signedTenantFixtureCall = entrytest.SignedTenantCall
	pilotPolicy             = entrytest.PilotPolicy
	entryPostgresDatabase   = entrytest.PostgresDatabase
	configurePostgresTenant = entrytest.ConfigurePostgresTenant
	entrySecretEnvironment  = entrytest.SecretEnvironment
)
