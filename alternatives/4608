package pgtransport_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func TestExplicitConnectionAuthority(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root.crt")
	if err := os.WriteFile(root, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	base := "postgres://example:fixture@db.example:6432/example?sslmode=verify-full"
	for _, test := range []struct {
		name, suffix string
		valid        bool
		mode         string
	}{
		{"explicit", "", true, "verify-full"},
		{"direct TLS", "&sslnegotiation=direct&sslrootcert=system", true, "verify-full"},
		{"cached mode overridden", "&default_query_exec_mode=cache_statement&statement_cache_capacity=200", true, "verify-full"},
		{"passfile", "&passfile=/example/credentials", false, "verify-full"},
		{"service", "&service=example", false, "verify-full"},
		{"session options", "&options=-c%20role%3Dother", false, "verify-full"},
		{"tenant override", "&detent.organization_id=other", false, "verify-full"},
		{"require without CA", "", true, "require"},
		{"require empty CA", "&sslrootcert=", true, "require"},
		{"require CA file", "&sslrootcert=" + url.QueryEscape(root), false, "require"},
		{"require system CA", "&sslrootcert=system", false, "require"},
		{"require direct TLS CA", "&sslnegotiation=direct&sslrootcert=" + url.QueryEscape(root), false, "require"},
		{"verify CA file", "&sslrootcert=" + url.QueryEscape(root), true, "verify-ca"},
		{"verify full CA file", "&sslrootcert=" + url.QueryEscape(root), true, "verify-full"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.Replace(base, "sslmode=verify-full", "sslmode="+test.mode, 1) + test.suffix
			config, err := pgtransport.ParsePoolConfig(dsn)
			if (err == nil) != test.valid {
				t.Fatalf("connection valid=%v err=%v", test.valid, err)
			}
			if !test.valid {
				if !errors.Is(err, pgtransport.ErrConnectionConfig) || config != nil {
					t.Fatalf("invalid connection returned config=%v err=%v", config != nil, err)
				}
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
			if test.mode == "verify-full" && (config.ConnConfig.TLSConfig.InsecureSkipVerify || config.ConnConfig.TLSConfig.ServerName != "db.example") {
				t.Fatal("full certificate and hostname verification changed")
			}
			if strings.Contains(test.suffix, "sslrootcert=") && !strings.HasSuffix(test.suffix, "sslrootcert=") && config.ConnConfig.TLSConfig.RootCAs == nil {
				t.Fatal("declared certificate authorities were not loaded")
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
