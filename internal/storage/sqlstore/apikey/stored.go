package apikey

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AnyStored reports whether the database holds an API key of any tenant.
// From then on the database owns API keys: startup imports config.yaml's
// api-keys and api-key-entries only into an empty table and otherwise drops
// them from the file.
func AnyStored(ctx context.Context, db *sql.DB) (bool, error) {
	return anyRow(ctx, db, "api_keys")
}

// AnyPermissionProfileStored is AnyStored for permission profiles, the
// condition under which config.yaml's api-key-permission-profiles are no
// longer imported.
func AnyPermissionProfileStored(ctx context.Context, db *sql.DB) (bool, error) {
	return anyRow(ctx, db, "api_key_permission_profiles")
}

// anyRow reads one row instead of counting them, so the check stays cheap on
// a large table. table is one of the constants above, never caller input.
func anyRow(ctx context.Context, db *sql.DB, table string) (bool, error) {
	if db == nil {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var one int
	err := db.QueryRowContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", table, err)
	}
	return true, nil
}
