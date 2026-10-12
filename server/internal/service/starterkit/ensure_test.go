package starterkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// These tests run against the developer database like the other DB suites here,
// and skip when it is unreachable so `go test ./...` stays usable without one.
// Each one works in its own fresh workspace and deletes what it created.

var (
	poolOnce sync.Once
	poolVal  *pgxpool.Pool
	poolErr  error
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		url := os.Getenv("DATABASE_URL")
		if url == "" {
			url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := pgxpool.New(ctx, url)
		if err != nil {
			poolErr = fmt.Errorf("database unavailable: %w", err)
			return
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			poolErr = fmt.Errorf("database unreachable: %w", err)
			return
		}
		poolVal = p
	})
	if poolErr != nil {
		t.Skip(poolErr)
	}
	return poolVal
}

// freshWorkspace builds the minimal real shape of a new workspace: a user, a
// workspace, and that user as its owner — the same three rows CreateWorkspace
// commits before its starter-kit hook runs.
func freshWorkspace(t *testing.T, f *testutil.Fixture, tag string) (workspaceID, ownerID pgtype.UUID) {
	t.Helper()
	n := nextFixture()
	ws := f.Workspace(t, "kit "+tag, fmt.Sprintf("kit-%s-%d", tag, n))
	usr := f.User(t, "owner "+tag, fmt.Sprintf("owner-%s-%d@example.com", tag, n))
	f.Member(t, ws, usr, "owner")

	// Parents first, children last: t.Cleanup runs in reverse, so skill_file,
	// agent_skill, and squad_member go before the rows they hang off. Nothing
	// cascades in this schema by design, so teardown is spelled out.
	//
	// Registered only, never run here: the workspace id comes from the counter
	// above, so there is nothing to pre-clean, and executing these statements
	// now would delete the owner row this helper has just inserted.
	for _, stmt := range []string{
		"DELETE FROM member WHERE workspace_id = $1",
		"DELETE FROM agent WHERE workspace_id = $1",
		"DELETE FROM skill WHERE workspace_id = $1",
		"DELETE FROM squad WHERE workspace_id = $1",
		"DELETE FROM starter_kit_seed WHERE workspace_id = $1",
		"DELETE FROM skill_file WHERE skill_id IN (SELECT id FROM skill WHERE workspace_id = $1)",
		"DELETE FROM agent_skill WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)",
		"DELETE FROM squad_member WHERE squad_id IN (SELECT id FROM squad WHERE workspace_id = $1)",
	} {
		f.Cleanup(t, stmt, ws)
	}
	f.Cleanup(t, "DELETE FROM workspace WHERE id = $1", ws)
	f.Cleanup(t, "DELETE FROM user WHERE id = $1", usr)

	return mustUUID(t, ws), mustUUID(t, usr)
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	u, err := util.ParseUUID(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

// kitCounts is every row the bundle owns in one workspace, read back from the
// database. Comparing these before and after a call is what "did nothing the
// second time" means here.
type kitCounts struct {
	skills, files, agents, bindings, squads, members, claims int
}

func countKit(t *testing.T, f *testutil.Fixture, ws pgtype.UUID) kitCounts {
	t.Helper()
	count := func(sql string) int {
		return f.Count(t, sql, ws)
	}
	var c kitCounts
	c.skills = count(`SELECT count(*) FROM skill WHERE workspace_id = $1`)
	c.files = count(`SELECT count(*) FROM skill_file f
		JOIN skill s ON s.id = f.skill_id WHERE s.workspace_id = $1`)
	c.agents = count(`SELECT count(*) FROM agent WHERE workspace_id = $1`)
	c.bindings = count(`SELECT count(*) FROM agent_skill a
		JOIN agent g ON g.id = a.agent_id WHERE g.workspace_id = $1`)
	c.squads = count(`SELECT count(*) FROM squad WHERE workspace_id = $1`)
	c.members = count(`SELECT count(*) FROM squad_member m
		JOIN squad q ON q.id = m.squad_id WHERE q.workspace_id = $1`)
	c.claims = count(`SELECT count(*) FROM starter_kit_seed WHERE workspace_id = $1`)
	return c
}

// bundleCounts is what the fixtures say a completely empty workspace should end
// up with, computed independently of the import.
func bundleCounts(t *testing.T) kitCounts {
	t.Helper()
	b, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var want kitCounts
	want.skills, want.agents, want.squads = len(b.Skills), len(b.Agents), len(b.Squads)
	for _, s := range b.Skills {
		want.files += len(s.Files)
	}
	for _, a := range b.Agents {
		want.bindings += len(a.Skills)
	}
	for _, sq := range b.Squads {
		// The leader is in the roster too, and squad_member holds one row per agent.
		want.members++
		for _, m := range sq.Members {
			if m.Agent != sq.Leader {
				want.members++
			}
		}
	}
	want.claims = 1
	return want
}

func TestEnsureImportsTheBundleOnce(t *testing.T) {
	pool := testPool(t)
	f := testutil.New(pool, "", "")
	ctx := context.Background()
	b, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ws, owner := freshWorkspace(t, f, "import")
	before := countKit(t, f, ws)
	if before != (kitCounts{}) {
		t.Fatalf("fresh workspace already has kit rows: %+v", before)
	}

	svc := New(db.New(pool), pool)
	res, err := svc.EnsureWorkspace(ctx, ws)
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if !res.Seeded {
		t.Fatal("EnsureWorkspace reported an empty result on an unclaimed workspace")
	}
	if res.Reused != 0 {
		t.Errorf("empty workspace reused %d existing rows", res.Reused)
	}
	got := countKit(t, f, ws)
	if want := bundleCounts(t); got != want {
		t.Errorf("imported row counts:\n got %+v\nwant %+v", got, want)
	}

	// The marker is the reason a restart is cheap and non-destructive.
	var key, version string
	f.QueryRow(t, `SELECT kit_key, kit_version FROM starter_kit_seed WHERE workspace_id = $1`, ws).
		Scan(&key, &version)
	if key != KitKey || version != KitVersion {
		t.Errorf("marker = (%q, %q), want (%q, %q)", key, version, KitKey, KitVersion)
	}

	// Imported rows belong to this workspace's owner, not to some user baked
	// into the fixtures.
	if n := f.Count(t, `SELECT count(*) FROM agent WHERE workspace_id = $1 AND owner_id = $2`, ws, owner); n != res.Agents {
		t.Errorf("%d of %d imported agents are owned by the workspace owner", n, res.Agents)
	}
	if n := f.Count(t, `SELECT count(*) FROM skill WHERE workspace_id = $1 AND created_by = $2`, ws, owner); n != res.Skills {
		t.Errorf("%d of %d imported skills are credited to the workspace owner", n, res.Skills)
	}

	t.Run("content survives the round trip", func(t *testing.T) {
		// Pick the richest skill and a multi-file skill: the byte comparison is
		// what proves frontmatter stripping and embedding lost nothing.
		var withFiles *SkillDef
		for i := range b.Skills {
			s := &b.Skills[i]
			if withFiles == nil || len(s.Files) > len(withFiles.Files) {
				withFiles = s
			}
		}
		if withFiles == nil || len(withFiles.Files) == 0 {
			t.Fatal("no bundled skill has files, so this test proves nothing")
		}
		var content, description string
		f.QueryRow(t, `SELECT content, description FROM skill WHERE workspace_id = $1 AND name = $2`,
			ws, withFiles.Name).Scan(&content, &description)
		if content != withFiles.Body {
			t.Errorf("skill %q: imported content is %d bytes, fixture body is %d",
				withFiles.Name, len(content), len(withFiles.Body))
		}
		if description != withFiles.Description {
			t.Errorf("skill %q: description = %q, want %q", withFiles.Name, description, withFiles.Description)
		}
		var fileContent string
		f.QueryRow(t, `SELECT f.content FROM skill_file f JOIN skill s ON s.id = f.skill_id
			WHERE s.workspace_id = $1 AND s.name = $2 AND f.path = $3`,
			ws, withFiles.Name, withFiles.Files[0]).Scan(&fileContent)
		if fileContent != withFiles.Blobs[withFiles.Files[0]] {
			t.Errorf("skill %q file %q: content differs from the fixture", withFiles.Name, withFiles.Files[0])
		}

		richest := b.Agents[0]
		for _, a := range b.Agents {
			if len(a.Instructions) > len(richest.Instructions) {
				richest = a
			}
		}
		var instructions string
		var avatar string
		f.QueryRow(t, `SELECT instructions, coalesce(avatar_url, '') FROM agent
			WHERE workspace_id = $1 AND name = $2`, ws, richest.Name).
			Scan(&instructions, &avatar)
		if instructions != richest.Instructions {
			t.Errorf("agent %q: instructions lost or altered in import", richest.Key)
		}
		if avatar != richest.AvatarURL {
			t.Errorf("agent %q: avatar = %q, want %q", richest.Key, avatar, richest.AvatarURL)
		}
	})

	t.Run("squad roster and leader point at the imported agents", func(t *testing.T) {
		sq := b.Squads[0]
		var leaderName string
		f.QueryRow(t, `SELECT a.name FROM squad s JOIN agent a ON a.id = s.leader_id
			WHERE s.workspace_id = $1 AND s.name = $2`, ws, sq.Name).Scan(&leaderName)
		if want := agentName(b, sq.Leader); leaderName != want {
			t.Errorf("squad %q leader = %q, want %q", sq.Name, leaderName, want)
		}
		// A leaderless or dangling roster row is the failure worth guarding:
		// every member must resolve inside this workspace.
		if n := f.Count(t, `SELECT count(*) FROM squad_member m JOIN squad s ON s.id = m.squad_id
			JOIN agent a ON a.id = m.member_id
			WHERE s.workspace_id = $1 AND s.name = $2 AND a.workspace_id = $1 AND m.member_type = 'agent'`,
			ws, sq.Name); n != int(countRoster(sq)) {
			t.Errorf("squad %q has %d resolvable agent members, want %d", sq.Name, n, countRoster(sq))
		}
	})

	t.Run("a second import does nothing", func(t *testing.T) {
		// The literal restart case: same process or a new one, the claim row is
		// the gate and no row may move.
		snapshot := countKit(t, f, ws)
		res, err := svc.EnsureWorkspace(ctx, ws)
		if !errors.Is(err, ErrNotClaimed) {
			t.Fatalf("second EnsureWorkspace err = %v, want ErrNotClaimed", err)
		}
		if res.Seeded {
			t.Errorf("second EnsureWorkspace reported seeding: %+v", res)
		}
		if after := countKit(t, f, ws); after != snapshot {
			t.Errorf("rows changed on a repeat import:\n before %+v\n after %+v", snapshot, after)
		}
	})

	t.Run("the next workspace gets its own copy", func(t *testing.T) {
		ws2, _ := freshWorkspace(t, f, "second")
		if _, err := svc.EnsureWorkspace(ctx, ws2); err != nil {
			t.Fatalf("EnsureWorkspace second workspace: %v", err)
		}
		if got, want := countKit(t, f, ws2), bundleCounts(t); got != want {
			t.Errorf("second workspace counts:\n got %+v\nwant %+v", got, want)
		}
		// Same names, different rows: nothing may be shared by id across
		// workspaces, or one team's edit would land on another's.
		if n := f.Count(t, `SELECT count(*) FROM skill a JOIN skill b ON a.name = b.name
			WHERE a.workspace_id = $1 AND b.workspace_id = $2 AND a.id = b.id`, ws, ws2); n != 0 {
			t.Errorf("%d skill ids are shared between two workspaces", n)
		}
	})
}

// TestEnsureLeavesExistingRowsAlone is the promise that makes a bundled kit safe
// beside a workspace's own work: a row that already answers to the bundle's name
// is kept as-is, its content and relationships untouched.
func TestEnsureLeavesExistingRowsAlone(t *testing.T) {
	pool := testPool(t)
	f := testutil.New(pool, "", "")
	ctx := context.Background()
	b, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ws, ownerID := freshWorkspace(t, f, "existing")

	skill := b.Skills[0]
	for _, cand := range b.Skills {
		if len(cand.Files) > len(skill.Files) {
			skill = cand
		}
	}
	if len(skill.Files) == 0 {
		t.Skip("no bundled skill has files, so the reuse case cannot be shown")
	}
	agent := b.Agents[0]
	squad := b.Squads[0]

	// The workspace did its own setup first: a skill it edited, an agent it
	// configured by hand, and a squad already named like the bundle's.
	skillID := f.Insert(t, "skill", testutil.Cols{
		"workspace_id": util.UUIDToString(ws), "name": skill.Name,
		"description": "ours", "content": "hand-written body",
	})
	agentID := f.Insert(t, "agent", testutil.Cols{
		"workspace_id": util.UUIDToString(ws), "name": agent.Name,
		"description": "ours", "runtime_mode": "local", "instructions": "hand-written notes",
	})
	squadID := f.Insert(t, "squad", testutil.Cols{
		"workspace_id": util.UUIDToString(ws), "name": squad.Name,
		"description": "ours", "leader_id": agentID, "creator_id": ownerID,
	})
	f.Cleanup(t, "DELETE FROM squad_member WHERE squad_id = $1", squadID)

	before := countKit(t, f, ws)
	res, err := New(db.New(pool), pool).EnsureWorkspace(ctx, ws)
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if !res.Seeded {
		t.Fatal("a workspace with only some of the bundle should still be claimed")
	}
	if res.Reused < 3 {
		t.Errorf("reused %d existing rows, want at least the 3 planted ones", res.Reused)
	}

	// Nothing may be added beside the three planted rows.
	after := countKit(t, f, ws)
	if got := f.Count(t, `SELECT count(*) FROM skill WHERE workspace_id = $1 AND name = $2`, ws, skill.Name); got != 1 {
		t.Errorf("%d skills named %q after import, want the one that already existed", got, skill.Name)
	}
	if got := f.Count(t, `SELECT count(*) FROM agent WHERE workspace_id = $1 AND name = $2`, ws, agent.Name); got != 1 {
		t.Errorf("%d agents named %q after import, want the one that already existed", got, agent.Name)
	}
	if got := f.Count(t, `SELECT count(*) FROM squad WHERE workspace_id = $1 AND name = $2`, ws, squad.Name); got != 1 {
		t.Errorf("%d squads named %q after import, want the one that already existed", got, squad.Name)
	}
	if after.skills != before.skills+res.Skills || after.agents != before.agents+res.Agents {
		t.Errorf("created-row accounting off: before %+v after %+v result %+v", before, after, res)
	}

	// And the existing rows kept their own content and their own emptiness: the
	// import does not backfill files, bindings, or members into a row it reused.
	var content string
	f.QueryRow(t, `SELECT content FROM skill WHERE id = $1`, skillID).Scan(&content)
	if content != "hand-written body" {
		t.Errorf("existing skill content was rewritten to %q", content)
	}
	f.QueryRow(t, `SELECT instructions FROM agent WHERE id = $1`, agentID).Scan(&content)
	if content != "hand-written notes" {
		t.Errorf("existing agent instructions were rewritten to %q", content)
	}
	if n := f.Count(t, `SELECT count(*) FROM skill_file WHERE skill_id = $1`, skillID); n != 0 {
		t.Errorf("import added %d files to a skill it reused", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM agent_skill WHERE agent_id = $1`, agentID); n != 0 {
		t.Errorf("import added %d skill bindings to an agent it reused", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM squad_member WHERE squad_id = $1`, squadID); n != 0 {
		t.Errorf("import added %d members to a squad it reused", n)
	}

	// The rows it did create are wired through the reused ones: a squad built by
	// the import must roster the workspace's existing agent, not a copy.
	if len(b.Squads) < 2 {
		t.Skip("bundle has one squad, so cross-wiring cannot be shown")
	}
	// The strongest form of reuse: a squad the import *did* create has to roster
	// the agent it found already standing, by that row's id.
	other := b.Squads[1]
	plantedIsInOther := agentName(b, other.Leader) == agent.Name
	for _, m := range other.Members {
		if m.Agent == agent.Key {
			plantedIsInOther = true
		}
	}
	if !plantedIsInOther {
		t.Skip("the planted agent is not in the second squad's bundle roster")
	}
	if n := f.Count(t, `SELECT count(*) FROM squad_member m JOIN squad s ON s.id = m.squad_id
		WHERE s.workspace_id = $1 AND s.name = $2 AND m.member_id = $3`,
		ws, other.Name, agentID); n != 1 {
		t.Errorf("squad %q rosters the pre-existing agent %q in %d rows, want 1",
			other.Name, agent.Name, n)
	}
}

// TestEnsureNeedsAnOwner covers the one input the bundle cannot supply: a member
// to author the imported rows. Failing without a claim is what keeps the next
// start able to finish the job.
func TestEnsureNeedsAnOwner(t *testing.T) {
	pool := testPool(t)
	f := testutil.New(pool, "", "")
	ctx := context.Background()

	ws, owner := freshWorkspace(t, f, "noowner")
	tag, err := f.Pool.Exec(ctx, `DELETE FROM member WHERE workspace_id = $1`, ws)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("could not remove the owner to build the ownerless case: %v (%d rows)", err, tag.RowsAffected())
	}
	usr := util.UUIDToString(owner)

	if _, err := New(db.New(pool), pool).EnsureWorkspace(ctx, ws); err == nil {
		t.Fatal("EnsureWorkspace succeeded with no owner to author the imported rows")
	} else if !strings.Contains(err.Error(), "no owner") {
		t.Fatalf("EnsureWorkspace err = %v, want the missing-owner case", err)
	}
	// The whole point of the shared transaction: nothing half-imported, and no
	// marker that would tell the next start to leave this workspace alone.
	if got := countKit(t, f, ws); got != (kitCounts{}) {
		t.Errorf("failed import left rows behind: %+v", got)
	}

	f.Member(t, util.UUIDToString(ws), usr, "owner")
	if _, err := New(db.New(pool), pool).EnsureWorkspace(ctx, ws); err != nil {
		t.Fatalf("EnsureWorkspace after the owner was restored: %v", err)
	}
	if got, want := countKit(t, f, ws), bundleCounts(t); got != want {
		t.Errorf("retried import counts:\n got %+v\nwant %+v", got, want)
	}
}

// TestSweepAllIsTheBackstop exercises the startup path: a workspace that never
// received the bundle gets it from the sweep, and running the sweep again —
// which is what every restart does — changes nothing.
func TestSweepAllIsTheBackstop(t *testing.T) {
	pool := testPool(t)
	f := testutil.New(pool, "", "")
	ctx := context.Background()

	first, _ := freshWorkspace(t, f, "sweep-a")
	svc := New(db.New(pool), pool)
	if _, err := svc.EnsureWorkspace(ctx, first); err != nil {
		t.Fatalf("seed the already-imported workspace: %v", err)
	}
	waiting, _ := freshWorkspace(t, f, "sweep-b")
	// A workspace whose create-workspace hook failed looks exactly like one that
	// predates the bundle: no marker, no rows.
	if got := countKit(t, f, waiting); got != (kitCounts{}) {
		t.Fatalf("workspace to be swept already has rows: %+v", got)
	}

	// The set is explicit so this cannot import anything into a workspace that
	// belongs to the developer rather than to the test.
	ids := []pgtype.UUID{first, waiting}
	svc.SweepWorkspaces(ctx, ids)

	if got, want := countKit(t, f, waiting), bundleCounts(t); got != want {
		t.Errorf("swept workspace counts:\n got %+v\nwant %+v", got, want)
	}
	before := countKit(t, f, first)
	svc.SweepWorkspaces(ctx, ids)
	svc.SweepWorkspaces(ctx, ids)
	if after := countKit(t, f, first); after != before {
		t.Errorf("repeat sweeps changed an already-imported workspace:\n before %+v\n after %+v", before, after)
	}
	if after := countKit(t, f, waiting); after != bundleCounts(t) {
		t.Errorf("repeat sweeps changed a swept workspace:\n want %+v\n got %+v", bundleCounts(t), after)
	}

	// SweepAll's own half of the job is the listing: a workspace must appear in
	// it, or the backstop would silently skip everyone.
	all, err := db.New(pool).ListAllWorkspaceIDs(ctx)
	if err != nil {
		t.Fatalf("ListAllWorkspaceIDs: %v", err)
	}
	var listed bool
	for _, id := range all {
		if id == waiting {
			listed = true
		}
	}
	if !listed {
		t.Errorf("ListAllWorkspaceIDs returned %d ids without the new workspace", len(all))
	}
}

// fixtureSeq keeps slugs and emails unique across parallel-ish tests in one run.
var fixtureSeq atomic.Int64

func nextFixture() int64 { return fixtureSeq.Add(1) }

func countRoster(sq SquadDef) int64 {
	n := int64(1) // the leader row
	for _, m := range sq.Members {
		if m.Agent != sq.Leader {
			n++
		}
	}
	return n
}

func agentName(b *Bundle, key string) string {
	for _, a := range b.Agents {
		if a.Key == key {
			return a.Name
		}
	}
	return ""
}
