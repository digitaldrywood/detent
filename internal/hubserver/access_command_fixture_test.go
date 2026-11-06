package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type accessCommandFault struct {
	stage, phase string
	err          error
	skip, calls  int
}

func (f *accessCommandFault) lookup(stage string) error {
	if f.stage != stage {
		return nil
	}
	f.calls++
	if f.calls <= f.skip {
		return nil
	}
	return f.err
}

type accessCommandFaultRepository struct {
	tenantstore.Repository
	fault *accessCommandFault
}

func (r accessCommandFaultRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		if r.fault.phase == "read" {
			reader = accessCommandFaultReader{Reader: reader, fault: r.fault}
		}
		return read(reader)
	})
}

func (r accessCommandFaultRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, workload, func(writer tenantstore.Writer) error {
		if r.fault.phase == "write" && r.fault.calls <= r.fault.skip {
			writer = accessCommandFaultWriter{Writer: writer, reader: accessCommandFaultReader{Reader: writer, fault: r.fault}}
		}
		return write(writer)
	})
}

type accessCommandFaultWriter struct {
	tenantstore.Writer
	reader accessCommandFaultReader
}

func (w accessCommandFaultWriter) ClaimReceipt(ctx context.Context, record receiptstore.Record) (receiptstore.Record, bool, error) {
	return w.Writer.(receiptstore.Writer).ClaimReceipt(ctx, record)
}
func (w accessCommandFaultWriter) CompleteReceipt(ctx context.Context, record receiptstore.Record) (receiptstore.Record, error) {
	return w.Writer.(receiptstore.Writer).CompleteReceipt(ctx, record)
}
func (w accessCommandFaultWriter) AbandonReceipt(ctx context.Context, actor, operation, key, hash string) error {
	return w.Writer.(receiptstore.Writer).AbandonReceipt(ctx, actor, operation, key, hash)
}
func (w accessCommandFaultWriter) ReceiptMember(ctx context.Context, actor string) (accessstore.Member, error) {
	return w.Writer.(receiptstore.ActorReader).ReceiptMember(ctx, actor)
}

func (w accessCommandFaultWriter) AccessTenant(ctx context.Context) (accessstore.Tenant, error) {
	return w.reader.AccessTenant(ctx)
}
func (w accessCommandFaultWriter) AccessInvitation(ctx context.Context, id string) (accessstore.Invitation, error) {
	return w.reader.AccessInvitation(ctx, id)
}
func (w accessCommandFaultWriter) ProjectRecord(ctx context.Context, id tracker.ProjectID) (projectstore.Record, error) {
	return w.reader.ProjectRecord(ctx, id)
}
func (w accessCommandFaultWriter) AccessProjectGrant(ctx context.Context, user string, id tracker.ProjectID) (accessstore.ProjectGrant, error) {
	return w.reader.AccessProjectGrant(ctx, user, id)
}
func (w accessCommandFaultWriter) AccessTokenGrant(ctx context.Context, token string, id tracker.ProjectID) (accessstore.TokenGrant, error) {
	return w.reader.AccessTokenGrant(ctx, token, id)
}
func (w accessCommandFaultWriter) AccessMember(ctx context.Context, user string) (accessstore.Member, error) {
	return w.reader.AccessMember(ctx, user)
}

type accessCommandFaultReader struct {
	tenantstore.Reader
	fault *accessCommandFault
}

func (r accessCommandFaultReader) AccessTenant(ctx context.Context) (accessstore.Tenant, error) {
	if err := r.fault.lookup("tenant"); err != nil {
		return accessstore.Tenant{}, err
	}
	return r.Reader.AccessTenant(ctx)
}

func (r accessCommandFaultReader) AccessInvitation(ctx context.Context, id string) (accessstore.Invitation, error) {
	if err := r.fault.lookup("invitation"); err != nil {
		return accessstore.Invitation{}, err
	}
	return r.Reader.AccessInvitation(ctx, id)
}

func (r accessCommandFaultReader) ProjectRecord(ctx context.Context, id tracker.ProjectID) (projectstore.Record, error) {
	if err := r.fault.lookup("project"); err != nil {
		return projectstore.Record{}, err
	}
	return r.Reader.ProjectRecord(ctx, id)
}

func (r accessCommandFaultReader) AccessProjectGrant(ctx context.Context, user string, id tracker.ProjectID) (accessstore.ProjectGrant, error) {
	if err := r.fault.lookup("project grant"); err != nil {
		return accessstore.ProjectGrant{}, err
	}
	return r.Reader.AccessProjectGrant(ctx, user, id)
}

func (r accessCommandFaultReader) AccessTokenGrant(ctx context.Context, token string, id tracker.ProjectID) (accessstore.TokenGrant, error) {
	if err := r.fault.lookup("token grant"); err != nil {
		return accessstore.TokenGrant{}, err
	}
	return r.Reader.AccessTokenGrant(ctx, token, id)
}

func (r accessCommandFaultReader) AccessMember(ctx context.Context, user string) (accessstore.Member, error) {
	if err := r.fault.lookup("actor"); err != nil {
		return accessstore.Member{}, err
	}
	return r.Reader.AccessMember(ctx, user)
}
