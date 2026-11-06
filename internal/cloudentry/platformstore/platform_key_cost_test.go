package platformstore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type platformWireCounts struct {
	mu                    sync.Mutex
	statements, exchanges int
}

type platformCountedConn struct {
	net.Conn
	counts  *platformWireCounts
	pending []byte
	startup bool
}

func (c *platformCountedConn) Write(value []byte) (int, error) {
	n, err := c.Conn.Write(value)
	c.pending = append(c.pending, value[:n]...)
	for {
		if c.startup {
			if len(c.pending) < 4 {
				break
			}
			size := int(binary.BigEndian.Uint32(c.pending[:4]))
			if len(c.pending) < size {
				break
			}
			c.pending, c.startup = c.pending[size:], false
			continue
		}
		if len(c.pending) < 5 {
			break
		}
		size := 1 + int(binary.BigEndian.Uint32(c.pending[1:5]))
		if len(c.pending) < size {
			break
		}
		c.counts.mu.Lock()
		switch c.pending[0] {
		case 'S':
			c.counts.exchanges++
		case 'Q':
			c.counts.exchanges++
			c.counts.statements++
		case 'E':
			c.counts.statements++
		}
		c.counts.mu.Unlock()
		c.pending = c.pending[size:]
	}
	return n, err
}

func TestPlatformKeyReadCostIsBounded(t *testing.T) {
	db := template.Database(t)
	initial := rolePool(t, db, postgres.PlatformRoleName, "")
	config := initial.Config()
	initial.Close()
	counts := new(platformWireCounts)
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &platformCountedConn{Conn: conn, counts: counts, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r, err := New(t.Context(), pool, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	seedOrganizations(t, r)
	if _, err := db.ExecContext(t.Context(), `UPDATE platform.organizations SET managed=true`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, size := range []int{2, 100} {
			id := fmt.Sprintf("key_%s_%d", org, size)
			projects := make([]tracker.ProjectID, size)
			for i := range projects {
				projects[i] = tracker.ProjectID(fmt.Sprintf("prj_%03d", i))
			}
			key := Key{ID: id, Hash: "hash_" + id, Name: "Example", Kind: "service", OwnerSubject: "owner", Permission: "read", AccessContext: "selected", ServiceOrganizationID: &org, Organizations: []KeyOrganization{{OrganizationID: org, ProjectAccess: "selected", Projects: projects}}}
			if err := r.CreateKey(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			for _, read := range []struct {
				name string
				run  func() error
			}{
				{"credential", func() error {
					stored, err := r.Key(t.Context(), key.Hash)
					if err != nil {
						return err
					}
					if len(stored.Organizations) != 1 || len(stored.Organizations[0].Projects) != size {
						return errors.New("stored scope was truncated")
					}
					return nil
				}},
				{"authority", func() error {
					_, err := r.KeyAuthority(t.Context(), key.Hash, org, projects[size-1])
					return err
				}},
				{"re-resolution", func() error {
					snapshot, err := r.TenantKey(t.Context(), key.ID, org)
					if err != nil {
						return err
					}
					if !snapshot.Authority.Allows(string(projects[size-1])) {
						return errors.New("authority scope was truncated")
					}
					return nil
				}},
			} {
				t.Run(fmt.Sprintf("%s/%d/%s", org, size, read.name), func(t *testing.T) {
					counts.mu.Lock()
					counts.statements, counts.exchanges = 0, 0
					counts.mu.Unlock()
					if err := read.run(); err != nil {
						t.Fatal(err)
					}
					counts.mu.Lock()
					statements, exchanges := counts.statements, counts.exchanges
					counts.mu.Unlock()
					if statements != 1 || exchanges != 1 {
						t.Errorf("statements=%d exchanges=%d, want 1 each", statements, exchanges)
					}
					t.Logf("projects=%d statements=%d exchanges=%d", size, statements, exchanges)
				})
			}
		}
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE platform.access_key_projects RENAME TO unavailable_key_projects`); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() error{
		func() error { _, err := r.Key(t.Context(), "hash_key_org_first_2"); return err },
		func() error { _, err := r.TenantKey(t.Context(), "key_org_first_2", "org_first"); return err },
	} {
		err := read()
		if !errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, tenantauth.ErrDenied) || errors.Is(err, ErrInvalid) {
			t.Errorf("query error became a denial: %v", err)
		}
	}
}
