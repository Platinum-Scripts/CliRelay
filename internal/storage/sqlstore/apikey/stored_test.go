package apikey

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAnyStoredSeesRowsOfEveryTenant(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)
	InitPermissionProfilesTable(db)
	ctx := context.Background()

	for name, check := range map[string]func(context.Context, *sql.DB) (bool, error){"api keys": AnyStored, "permission profiles": AnyPermissionProfileStored} {
		if stored, err := check(ctx, db); stored || err != nil {
			t.Fatalf("%s of an empty database = %v, %v", name, stored, err)
		}
		if stored, err := check(ctx, nil); stored || err != nil {
			t.Fatalf("%s without a database = %v, %v", name, stored, err)
		}
	}
	tenant := NewTenantStore(db, "11111111-1111-1111-1111-111111111111")
	if err := tenant.ReplaceAll([]APIKeyRow{{Key: "sk-tenant-key-0000000001", Name: "tenant"}}); err != nil {
		t.Fatal(err)
	}
	if err := tenant.ReplaceAllPermissionProfiles([]PermissionProfileRow{{ID: "tenant-profile", Name: "Tenant"}}); err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]func(context.Context, *sql.DB) (bool, error){"api keys": AnyStored, "permission profiles": AnyPermissionProfileStored} {
		if stored, err := check(ctx, db); !stored || err != nil {
			t.Fatalf("%s with a row of another tenant = %v, %v; want true", name, stored, err)
		}
	}

	_ = db.Close()
	if _, err := AnyStored(ctx, db); err == nil {
		t.Fatal("a failed query must be reported, not read as no keys")
	}
}
