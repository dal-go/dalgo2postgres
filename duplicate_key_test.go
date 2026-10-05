package dalgo2postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsAlreadyExists proves the three contractual states of the
// PostgreSQL duplicate-key classifier wired into DbOptions.IsAlreadyExists
// (see database.go's NewDatabaseWithOptions): a genuine unique-violation
// (SQLSTATE 23505) is classified as a duplicate, a sibling class-23 code
// that is NOT a duplicate key (23503 foreign_key_violation, 23514
// check_violation) is not, and an unrelated error is not either. These run
// against a synthetic *pgconn.PgError and need no live PostgreSQL server.
func TestIsAlreadyExists(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "23505 unique_violation is a duplicate key",
			err:  &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"},
			want: true,
		},
		{
			name: "23505 wrapped by fmt.Errorf is still recognized via errors.As",
			err:  fmt.Errorf("exec insert: %w", &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}),
			want: true,
		},
		{
			name: "23503 foreign_key_violation is NOT a duplicate key",
			err:  &pgconn.PgError{Code: "23503", Message: "insert or update on table violates foreign key constraint"},
			want: false,
		},
		{
			name: "23514 check_violation is NOT a duplicate key",
			err:  &pgconn.PgError{Code: "23514", Message: "new row violates check constraint"},
			want: false,
		},
		{
			name: "unrelated PgError code is not a duplicate key",
			err:  &pgconn.PgError{Code: "42P01", Message: "relation does not exist"},
			want: false,
		},
		{
			name: "a plain non-PgError error is not a duplicate key",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "nil error is not a duplicate key",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAlreadyExists(tt.err); got != tt.want {
				t.Errorf("IsAlreadyExists(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestApplyPostgresDbOptionDefaults proves the exact production defaulting
// logic NewDatabaseWithOptions runs on every call (via
// applyPostgresDbOptionDefaults): a nil DbOptions.IsAlreadyExists is
// defaulted to this package's pgx-based IsAlreadyExists so
// Insert/InsertMulti reject a unique-key violation with
// record.IsAlreadyExists without the caller having to wire the hook up by
// hand, while a caller-supplied hook is preserved untouched rather than
// overridden. This calls the real helper directly — not a re-implementation
// — so it needs no live PostgreSQL connection.
func TestApplyPostgresDbOptionDefaults(t *testing.T) {
	t.Run("nil hook is defaulted to the package classifier", func(t *testing.T) {
		opts := dalgo2sql.DbOptions{}
		applyPostgresDbOptionDefaults(&opts)
		if opts.IsAlreadyExists == nil {
			t.Fatal("expected IsAlreadyExists to be defaulted, got nil")
		}
		if !opts.IsAlreadyExists(&pgconn.PgError{Code: "23505"}) {
			t.Error("expected the defaulted hook to classify a 23505 error as a duplicate")
		}
	})

	t.Run("caller-supplied hook is preserved", func(t *testing.T) {
		called := false
		opts := dalgo2sql.DbOptions{
			IsAlreadyExists: func(error) bool {
				called = true
				return false
			},
		}
		applyPostgresDbOptionDefaults(&opts)
		_ = opts.IsAlreadyExists(errors.New("anything"))
		if !called {
			t.Error("expected the caller-supplied hook to be preserved, but the default replaced it")
		}
	})

	t.Run("the structured query dialect is always forced to postgres", func(t *testing.T) {
		// "" is the legacy text emitter, "sqlite" and "mysql" are other emitters, and
		// "nonsense" is refused by dalgo2sql at read time: none may survive.
		for _, dialect := range []string{"", "sqlite", "mysql", "nonsense", "postgres"} {
			opts := dalgo2sql.DbOptions{StructuredQueryDialect: dialect}
			applyPostgresDbOptionDefaults(&opts)
			if opts.StructuredQueryDialect != "postgres" {
				t.Errorf("StructuredQueryDialect %q became %q, want postgres", dialect, opts.StructuredQueryDialect)
			}
		}
	})

	t.Run("Placeholder is always forced to the dollar dialect", func(t *testing.T) {
		opts := dalgo2sql.DbOptions{Placeholder: dalgo2sql.PlaceholderQuestion}
		applyPostgresDbOptionDefaults(&opts)
		if opts.Placeholder != dalgo2sql.PlaceholderDollar {
			t.Errorf("expected Placeholder to be forced to PlaceholderDollar, got %v", opts.Placeholder)
		}
	})
}
