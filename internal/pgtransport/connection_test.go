package pgtransport_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func TestExplicitConnectionAuthority(t *testing.T) {
	base := "postgres://example:fixture@db.example:6432/example?sslmode=verify-full"
	for _, test := range []struct {
		name, suffix string
		valid        bool
	}{
		{"explicit", "", true},
		{"direct TLS", "&sslnegotiation=direct&sslrootcert=system", true},
		{"cached mode overridden", "&default_query_exec_mode=cache_statement&statement_cache_capacity=200", true},
		{"passfile", "&passfile=/example/credentials", false},
		{"service", "&service=example", false},
		{"session options", "&options=-c%20role%3Dother", false},
		{"tenant override", "&detent.organization_id=other", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := pgtransport.ParsePoolConfig(base + test.suffix)
			if (err == nil) != test.valid {
				t.Fatalf("connection valid=%v err=%v", test.valid, err)
			}
			if !test.valid {
				return
			}
			if config.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec || config.ConnConfig.StatementCacheCapacity != 0 || config.ConnConfig.DescriptionCacheCapacity != 0 {
				t.Fatal("native execution is not enforced")
			}
			if config.ConnConfig.Password != "fixture" || config.ConnConfig.User != "example" || config.ConnConfig.Port != 6432 {
				t.Fatal("explicit connection authority changed")
			}
			if len(config.ConnConfig.TLSConfig.Certificates) != 0 {
				t.Fatal("undeclared client certificate was loaded")
			}
		})
	}
	for _, key := range []string{"PGPASSFILE", "PGSERVICE", "PGOPTIONS", "PGPASSWORD"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "example-untrusted")
			if _, err := pgtransport.ParsePoolConfig(base); !errors.Is(err, pgtransport.ErrConnectionConfig) {
				t.Fatalf("ambient setting accepted: %v", err)
			}
		})
	}

}
