package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func conversationLifecyclePostgresOwner(t *testing.T, f conversationAPIFixture, now time.Time) accessstore.Credential {
	t.Helper()
	return accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: f.ownerID, Name: "Example owner", Hash: "example-owner-hash", Scope: "operator", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}
}

type lifecycleSignalCoordinator struct {
	wakes chan<- string
}

func (c lifecycleSignalCoordinator) Available() bool           { return true }
func (c lifecycleSignalCoordinator) Wake(id string)            { c.wakes <- id }
func (lifecycleSignalCoordinator) Cancel(string) bool          { return false }
func (lifecycleSignalCoordinator) Hold(string) (func(), error) { return func() {}, nil }
func (lifecycleSignalCoordinator) Stop()                       {}

type lifecycleSignalControls struct{}

func (lifecycleSignalControls) Wake(string) {}
func (lifecycleSignalControls) Stop()       {}

func newConversationTenantFixture(t *testing.T, repository tenantstore.Repository, now time.Time) conversationAPIFixture {
	t.Helper()
	service := &Service{
		config:   Config{TenantRepository: repository, AccessRepository: repository, Logger: discardLogger(), Clock: func() time.Time { return now }, Hosted: &hostedplans.HostedConfig{OrganizationID: "org_conversation"}},
		database: &database{}, notifications: newNotificationBroker(),
	}
	service.ready.Store(true)
	chat := &conversationService{Server: service.conversationServer(), Store: &conversationStore{Contents: &conversationContentFixture{}}, Broker: service.notifications.Broker, Logger: service.config.Logger, Config: (ConversationConfig{Enabled: true}).Normalized(), Settle: make(chan struct{}), Settled: make(chan struct{})}
	close(chat.Settled)
	chat.Coordinator = newConversationCoordinator(service, chat)
	chat.Controls = conversations.NewWorkerRouter(chat)
	service.conversations = chat
	t.Cleanup(service.conversations.Stop)
	return conversationAPIFixture{nativeFixture: nativeFixture{service: service, project: tracker.NativeProject{OrganizationID: "org_conversation", ID: "prj_conversation", Name: "Example project", Slug: "example-project", Profile: "native", States: nativeFixtureStates()}}, ownerID: "owner_conversation"}
}
