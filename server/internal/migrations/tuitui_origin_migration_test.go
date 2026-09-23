package migrations

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tuituiOriginMigrationTestSchema = "tuitui_origin_migration_test"

// TestTuituiOriginMigrationsUpDownAndCatalog pins the 510/511 widen-CHECK pair
// the Tuitui /issue command depends on, the same way
// TestTelegramOriginMigrationsUpDownAndCatalog pins 366/367: the pre-Tuitui
// constraint must reject the label, 510 must admit it while unvalidated, 511
// must validate without rebuilding the lock, and the two downs must restore
// the earlier state — including that 'telegram_chat' survives, since a
// respecified CHECK drops every value it omits.
func TestTuituiOriginMigrationsUpDownAndCatalog(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire Postgres connection: %v", err)
	}
	defer conn.Release()

	cleanup := func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+tuituiOriginMigrationTestSchema+" CASCADE")
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+tuituiOriginMigrationTestSchema); err != nil {
		t.Fatalf("create isolated migration schema: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT set_config('search_path', $1, false)`, tuituiOriginMigrationTestSchema); err != nil {
		t.Fatalf("set isolated migration search path: %v", err)
	}

	// The state migrations 366/367 leave behind: validated, Telegram known,
	// Tuitui not.
	if _, err := conn.Exec(ctx, `
		CREATE TABLE issue (
			id UUID PRIMARY KEY,
			origin_type TEXT NULL
		);
		ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
			CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat', 'telegram_chat'));
	`); err != nil {
		t.Fatalf("create pre-Tuitui issue table: %v", err)
	}

	assertTuituiOriginRejected(t, ctx, conn.Conn(), "00000000-0000-4000-8000-000000000001")

	applyMigrationFile(t, ctx, conn.Conn(), "510_issue_origin_tuitui_chat.up.sql")
	assertTuituiOriginConstraint(t, ctx, conn.Conn(), false, true)
	if _, err := conn.Exec(ctx, `INSERT INTO issue (id, origin_type) VALUES ($1, 'tuitui_chat')`, "00000000-0000-4000-8000-000000000002"); err != nil {
		t.Fatalf("insert tuitui_chat after widening constraint: %v", err)
	}

	applyMigrationFile(t, ctx, conn.Conn(), "511_issue_origin_tuitui_chat_validate.up.sql")
	assertTuituiOriginConstraint(t, ctx, conn.Conn(), true, true)

	applyMigrationFile(t, ctx, conn.Conn(), "511_issue_origin_tuitui_chat_validate.down.sql")
	assertTuituiOriginConstraint(t, ctx, conn.Conn(), false, true)
	if _, err := conn.Exec(ctx, `DELETE FROM issue WHERE origin_type = 'tuitui_chat'`); err != nil {
		t.Fatalf("remove Tuitui row before narrowing rollback: %v", err)
	}
	applyMigrationFile(t, ctx, conn.Conn(), "510_issue_origin_tuitui_chat.down.sql")
	assertTuituiOriginConstraint(t, ctx, conn.Conn(), true, false)
	assertTuituiOriginRejected(t, ctx, conn.Conn(), "00000000-0000-4000-8000-000000000003")
}

func assertTuituiOriginConstraint(t *testing.T, ctx context.Context, conn *pgx.Conn, wantValidated, wantTuitui bool) {
	t.Helper()
	var validated bool
	var definition string
	if err := conn.QueryRow(ctx, `
		SELECT convalidated, pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'issue'::regclass AND conname = 'issue_origin_type_check'
	`).Scan(&validated, &definition); err != nil {
		t.Fatalf("inspect issue origin constraint: %v", err)
	}
	if validated != wantValidated {
		t.Fatalf("constraint validated = %t, want %t", validated, wantValidated)
	}
	if strings.Contains(definition, "tuitui_chat") != wantTuitui {
		t.Fatalf("constraint Tuitui membership = %t, want %t: %s", strings.Contains(definition, "tuitui_chat"), wantTuitui, definition)
	}
	// Narrowing to restore the pre-Tuitui list must not silently narrow
	// further: Telegram rides the same constraint.
	if wantTuitui && !strings.Contains(definition, "telegram_chat") {
		t.Fatalf("widened constraint lost the Telegram origin: %s", definition)
	}
}

func assertTuituiOriginRejected(t *testing.T, ctx context.Context, conn *pgx.Conn, id string) {
	t.Helper()
	if _, err := conn.Exec(ctx, `INSERT INTO issue (id, origin_type) VALUES ($1, 'tuitui_chat')`, id); !isCheckViolation(err) {
		t.Fatalf("insert tuitui_chat under pre-Tuitui constraint: got %v, want check violation", err)
	}
}
