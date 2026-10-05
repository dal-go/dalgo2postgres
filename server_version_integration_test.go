package dalgo2postgres

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// envExpectedMajor is the major version of the server the job claims to run against
// (the Conformance job sets it from its matrix). When it is set, the test below fails if the
// server is another, so a leg of the matrix cannot pass on the wrong image.
const envExpectedMajor = "DALGO2POSTGRES_EXPECT_SERVER_MAJOR"

// The server's own version string goes to the output of every run, so the evidence of the
// type matrix, the pins and the catalog tests names the server it comes from.
func TestServerVersionIntegration_IsTheOneTheJobNames(t *testing.T) {
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()
	var version, major string
	if err := db.QueryRowContext(context.Background(),
		`SELECT version(), (current_setting('server_version_num')::int / 10000)::text`).Scan(&version, &major); err != nil {
		t.Fatalf("version: %v", err)
	}
	t.Logf("server: %s (server_version_num major %s)", version, major)
	if !strings.HasPrefix(version, "PostgreSQL ") {
		t.Errorf("version() = %q, want PostgreSQL", version)
	}
	if want := os.Getenv(envExpectedMajor); want != "" && major != want {
		t.Errorf("the server's major version is %s, the job names %s", major, want)
	}
}
