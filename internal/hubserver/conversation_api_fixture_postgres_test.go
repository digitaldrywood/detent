package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
)

func (f *conversationWorkerFixture) replay(t *testing.T) ([]conversation.Event, error) {
	t.Helper()
	fixture := conversationFixture{nativeFixture: f.nativeFixture, store: f.chat.store, organization: f.project.OrganizationID}
	return fixture.publishedRecordEvents(t.Context(), f.record, 0)
}
