package routing

import (
	"context"
	"database/sql"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "modernc.org/sqlite"
)

// Load tells a tenant without routing from a row it cannot read, which
// GetWithVersion reports alike; a config.yaml save must not mistake the one
// for the other and let a section through that the reload would discard.
func TestLoadReportsARowItCannotRead(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)
	store := NewStore(db)
	ctx := context.Background()

	if stored, version, err := store.Load(ctx); stored != nil || version != 0 || err != nil {
		t.Fatalf("Load without a row = %+v, %d, %v; want nil, 0, nil", stored, version, err)
	}
	if err := store.Upsert(config.RoutingConfig{Strategy: "fill-first", IncludeDefaultGroup: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if stored, version, err := store.Load(ctx); err != nil || version != 1 || stored == nil || stored.Strategy != "fill-first" {
		t.Fatalf("Load = %+v, %d, %v; want fill-first at version 1", stored, version, err)
	}

	if _, err := db.Exec(`UPDATE routing_config SET payload = '{not json'`); err != nil {
		t.Fatal(err)
	}
	stored, version, err := store.Load(ctx)
	if err == nil || stored != nil || version != 1 {
		t.Fatalf("Load of a payload that does not decode = %+v, %d, %v; want nil, 1 and an error", stored, version, err)
	}
	if stored, version := store.GetWithVersion(); stored != nil || version != 1 {
		t.Fatalf("GetWithVersion = %+v, %d; want nil, 1 as before", stored, version)
	}

	_ = db.Close()
	if _, _, err := store.Load(ctx); err == nil {
		t.Fatal("a failed query must be reported, not read as no routing")
	}
}
