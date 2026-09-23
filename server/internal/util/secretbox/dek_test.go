package secretbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The deployment env var is still the first source, so these tests name their own
// variables and never inherit an operator's MULTICA_*_SECRET_KEY.
const (
	dekTestEnvVar   = "MULTICA_SECRETBOX_TEST_KEY"
	dekTestNoEnvVar = "MULTICA_SECRETBOX_TEST_ABSENT_KEY"
)

func TestResolveIntegrationKeyPrefersEnvironment(t *testing.T) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv(dekTestEnvVar, base64.StdEncoding.EncodeToString(key))

	material, err := ResolveIntegrationKey(context.Background(), dekTestEnvVar, nil)
	if err != nil {
		t.Fatalf("ResolveIntegrationKey: %v", err)
	}
	if string(material.Key) != string(key) {
		t.Fatal("resolved key is not the env key")
	}
	if material.Box == nil {
		t.Fatal("Material.Box is nil; callers build nothing else")
	}
	// The env path keeps LoadKey's validation, so a malformed value is an error
	// rather than a silently different key.
	t.Setenv(dekTestEnvVar, "not base64!")
	if _, err := ResolveIntegrationKey(context.Background(), dekTestEnvVar, nil); err == nil {
		t.Fatal("malformed env key resolved without error")
	}
}

func TestResolveIntegrationKeyWithoutAnySource(t *testing.T) {
	t.Setenv(dekTestEnvVar, "")
	if _, err := ResolveIntegrationKey(context.Background(), dekTestEnvVar, nil); err == nil {
		t.Fatal("expected an error when neither the env var nor queries are available")
	} else if !errors.Is(err, ErrNoKeySource) {
		t.Fatalf("error = %v, want ErrNoKeySource", err)
	}
}

func TestResolveIntegrationKeyGeneratesOneSharedDEK(t *testing.T) {
	q := dekTestQueries(t)
	ctx := context.Background()
	// Force a first boot. Nothing else in the suite reads this row, and a
	// deployment that already minted one keeps its key: only this test deletes.
	if _, err := dekPool.Exec(ctx, `DELETE FROM instance_secret WHERE key_name = $1`, IntegrationDEKName); err != nil {
		t.Fatalf("clear DEK: %v", err)
	}

	// Concurrent first boot: every node generates its own candidate, but the
	// winner's row is what everyone re-reads. Two keys in one deployment would
	// leave half the stored ciphertext permanently unopenable, so this is the
	// property the whole feature rests on.
	const racers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		resolved [][]byte
		errs     []error
	)
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			material, err := ResolveIntegrationKey(ctx, dekTestNoEnvVar, q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			resolved = append(resolved, material.Key)
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("ResolveIntegrationKey errors: %v", errs)
	}
	if len(resolved) != racers {
		t.Fatalf("resolved %d of %d", len(resolved), racers)
	}
	for i, key := range resolved {
		if len(key) != KeySize {
			t.Fatalf("resolved key %d is %d bytes, want %d", i, len(key), KeySize)
		}
		if string(key) != string(resolved[0]) {
			t.Fatalf("resolved key %d differs from key 0; first boot minted more than one DEK", i)
		}
	}
	var rowCount int
	if err := dekPool.QueryRow(ctx,
		`SELECT count(*) FROM instance_secret WHERE key_name = $1`, IntegrationDEKName).
		Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("instance_secret rows for %s = %d, want 1", IntegrationDEKName, rowCount)
	}

	// The stored key keeps working across processes: a fresh Material built later
	// opens what an earlier one sealed.
	sealer, err := ResolveIntegrationKey(ctx, dekTestEnvVar, q)
	if err != nil {
		t.Fatalf("second ResolveIntegrationKey: %v", err)
	}
	sealed, err := sealer.Box.Seal([]byte("tuitui-app-secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	opener, err := ResolveIntegrationKey(ctx, dekTestEnvVar, q)
	if err != nil {
		t.Fatalf("third ResolveIntegrationKey: %v", err)
	}
	opened, err := opener.Box.Open(sealed)
	if err != nil {
		t.Fatalf("Open with a re-resolved key: %v", err)
	}
	if string(opened) != "tuitui-app-secret" {
		t.Fatalf("opened %q", opened)
	}
}

var dekPool *pgxpool.Pool

// dekTestQueries connects to the same database the Go test suites use and skips
// the test when it is unreachable, so `go test ./internal/util/secretbox/` still
// works in an environment with no Postgres.
func dekTestQueries(t *testing.T) *db.Queries {
	t.Helper()
	if dekPool == nil {
		dbURL := os.Getenv("DATABASE_URL")
		if dbURL == "" {
			dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
		}
		pool, err := pgxpool.New(context.Background(), dbURL)
		if err != nil {
			t.Skipf("could not connect to database: %v", err)
		}
		if err := pool.Ping(context.Background()); err != nil {
			pool.Close()
			t.Skipf("database not reachable: %v", err)
		}
		dekPool = pool
	}
	return db.New(dekPool)
}
