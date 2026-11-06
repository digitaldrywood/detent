package postgres

import (
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func eventRecord(row pgquery.CollaborationEvent) eventstore.Event {
	return eventstore.Event{PushKnown: row.PushKnown, PushChangeID: row.PushChangeID, PushVersionID: row.PushVersionID, PushHeadSHA: row.PushHeadSha, PushExternalEffect: row.PushExternalEffect, PushEffectState: row.PushEffectState, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ID: row.ID, Sequence: row.Sequence, Type: row.Type, SchemaVersion: row.SchemaVersion, ActorKind: row.ActorKind, ActorPrincipalID: row.ActorPrincipalID, EventRevision: row.EventRevision, ChangeID: row.ChangeID, VersionID: row.VersionID, HeadSHA: row.HeadSHA, RunID: row.RunID, AttemptID: row.AttemptID, FromState: row.FromState, ToState: row.ToState, RecordedAt: row.RecordedAt, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes, PayloadFormatVersion: row.PayloadFormatVersion, LandingWaiting: row.LandingWaiting, LandingChangeID: row.LandingChangeID, LandingVersionID: row.LandingVersionID, LandingHeadSHA: row.LandingHeadSHA, ProjectSequence: row.ProjectSequence}
}

func evidenceRecord(row pgquery.ChangeEvidence) eventstore.Evidence {
	return eventstore.Evidence{ReviewKind: row.ReviewKind, ReviewLanding: row.ReviewLanding, ReviewSessionKnown: row.ReviewSessionKnown, ReviewSessionID: row.ReviewSessionID, ReviewSessionMachine: tracker.MachineID(row.ReviewSessionMachine), OrganizationID: row.OrganizationID, ChangeID: row.ChangeID, Sequence: row.Sequence, VersionID: row.VersionID, Kind: row.Kind, SourceKey: row.SourceKey, ActorKind: row.ActorKind, ActorPrincipalID: row.ActorPrincipalID, Decision: row.Decision, CreatedAt: row.CreatedAt, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes, PayloadFormatVersion: row.PayloadFormatVersion}
}
