package starterkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TxStarter opens the transaction an import runs in. Same shape the other
// services take (internal/service/autopilot.go), satisfied by the pool.
type TxStarter interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Service binds the bundle to a database for callers that do not manage a
// transaction themselves: the startup sweep, and tests.
type Service struct {
	Queries   *db.Queries
	TxStarter TxStarter
}

func New(queries *db.Queries, txStarter TxStarter) *Service {
	return &Service{Queries: queries, TxStarter: txStarter}
}

// EnsureWorkspace imports the bundle in its own transaction and commits it.
func (s *Service) EnsureWorkspace(ctx context.Context, workspaceID pgtype.UUID) (Result, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("starterkit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, err := Ensure(ctx, s.Queries.WithTx(tx), workspaceID)
	if err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("starterkit: commit: %w", err)
	}
	return res, nil
}

// SweepAll imports the bundle into every workspace that has never received it.
// It exists for the restart case: a bundle added after workspaces already
// existed, a create-workspace hook that failed, a database restored from before
// this code shipped. One claim query per workspace does the filtering, so a
// steady-state deployment pays a handful of indexed selects at startup.
func (s *Service) SweepAll(ctx context.Context) {
	ids, err := s.Queries.ListAllWorkspaceIDs(ctx)
	if err != nil {
		slog.Warn("starterkit: could not list workspaces", "error", err)
		return
	}
	s.SweepWorkspaces(ctx, ids)
}

// SweepWorkspaces is SweepAll over an explicit set, split out so tests can drive
// the real startup path without importing anything into workspaces that belong
// to other people. Failures are logged and skipped: an unseeded workspace is a
// degraded one, not a reason to refuse to serve another.
func (s *Service) SweepWorkspaces(ctx context.Context, ids []pgtype.UUID) {
	var seeded, skipped int
	for _, id := range ids {
		res, err := s.EnsureWorkspace(ctx, id)
		logResult(id, res, err)
		switch {
		case errors.Is(err, ErrNotClaimed):
			skipped++
		case err == nil && res.Seeded:
			seeded++
		}
	}
	if seeded > 0 || len(ids) == 0 {
		slog.Info("starterkit: startup sweep done",
			"workspaces", len(ids), "seeded", seeded, "already_seeded", skipped)
	}
}

// logResult reports one import. ErrNotClaimed is the expected steady state, so it
// stays silent; everything else is either the interesting first run or a problem
// an operator should see. No request is in flight — these run off the HTTP path
// — so the lines carry the workspace id rather than request attrs.
func logResult(workspaceID pgtype.UUID, res Result, err error) {
	switch {
	case errors.Is(err, ErrNotClaimed):
	case err != nil:
		slog.Warn("starterkit: import failed",
			"workspace_id", uuidText(workspaceID), "error", err)
	case res.Seeded:
		slog.Info("starterkit: imported bundled skills, agents, and squads",
			"workspace_id", uuidText(workspaceID),
			"skills", res.Skills, "skill_files", res.SkillFiles,
			"agents", res.Agents, "bindings", res.Bindings,
			"squads", res.Squads, "squad_members", res.SquadMembers,
			"reused_existing", res.Reused)
	}
}

// ErrNotClaimed reports that the workspace already carries the bundle, so this
// call did nothing. It is the expected result on every run after the first.
var ErrNotClaimed = errors.New("starterkit: workspace already seeded")

// Ensure imports the bundle into one workspace on the caller's transaction.
//
// CONTRACT: run this inside a transaction you commit only on success. The claim
// row is undone by that rollback, which is the entire retry story: an import
// that fails halfway leaves no marker, so the next start finishes the job.
// Called on a bare *db.Queries, the marker commits on its own and a later
// failure leaves the workspace permanently recorded as seeded. Use
// Service.EnsureWorkspace, which owns the transaction, unless you are already in
// one — as CreateWorkspace is for the issue status catalog.
//
// The claim is the first statement, and it is deliberately not a read-then-write:
// two pods starting against the same fresh database race one INSERT ... ON
// CONFLICT, the loser blocks on the winner's open transaction and then inserts
// nothing, so exactly one of them does the work and neither needs a lock. The
// loser's Ensure returns ErrNotClaimed with a rolled-back transaction, which is
// also why callers must treat it as success rather than retry.
//
// Rows that already exist under a bundle name are reused, never rewritten: the
// import resolves them to their current ids so bindings and rosters still point
// at the workspace's own rows.
func Ensure(ctx context.Context, q Querier, workspaceID pgtype.UUID) (Result, error) {
	claimed, err := q.ClaimStarterKitSeed(ctx, db.ClaimStarterKitSeedParams{
		WorkspaceID: workspaceID,
		KitKey:      KitKey,
		KitVersion:  KitVersion,
	})
	if err != nil {
		return Result{}, fmt.Errorf("starterkit: claim: %w", err)
	}
	if claimed == 0 {
		return Result{}, ErrNotClaimed
	}

	b, err := Load()
	if err != nil {
		return Result{}, err
	}

	owner, err := q.GetWorkspaceOwnerUserID(ctx, workspaceID)
	if err != nil {
		// No owner means no author for the imported rows. Leaving no marker
		// behind is the point: this workspace is retried on the next start
		// instead of being permanently half-configured.
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, fmt.Errorf("starterkit: workspace %s has no owner", uuidText(workspaceID))
		}
		return Result{}, fmt.Errorf("starterkit: workspace owner: %w", err)
	}

	res := Result{Seeded: true}
	skillIDs := make(map[string]pgtype.UUID, len(b.Skills))
	agentIDs := make(map[string]pgtype.UUID, len(b.Agents))

	if skillIDs, err = ensureSkills(ctx, q, workspaceID, owner, b, &res); err != nil {
		return res, err
	}
	if agentIDs, err = ensureAgents(ctx, q, workspaceID, owner, b, skillIDs, &res); err != nil {
		return res, err
	}
	if err := ensureSquads(ctx, q, workspaceID, owner, b, agentIDs, &res); err != nil {
		return res, err
	}
	return res, nil
}

func ensureSkills(ctx context.Context, q Querier, workspaceID, owner pgtype.UUID,
	b *Bundle, res *Result) (map[string]pgtype.UUID, error) {

	ids := make(map[string]pgtype.UUID, len(b.Skills))
	for _, def := range b.Skills {
		existing, err := q.GetSkillByWorkspaceAndName(ctx, db.GetSkillByWorkspaceAndNameParams{
			WorkspaceID: workspaceID,
			Name:        def.Name,
		})
		if err == nil {
			ids[def.Name] = existing.ID
			res.Reused++
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("starterkit: look up skill %q: %w", def.Name, err)
		}

		skill, err := q.CreateSkill(ctx, db.CreateSkillParams{
			WorkspaceID: workspaceID,
			Name:        def.Name,
			Description: def.Description,
			Content:     def.Body,
			// The importer's own contract: config is the workspace's to edit,
			// so the bundle starts it empty rather than inventing a default.
			Config:    []byte("{}"),
			CreatedBy: owner,
		})
		if err != nil {
			return nil, fmt.Errorf("starterkit: create skill %q: %w", def.Name, err)
		}
		ids[def.Name] = skill.ID
		res.Skills++

		for _, rel := range def.Files {
			if _, err := q.UpsertSkillFile(ctx, db.UpsertSkillFileParams{
				SkillID: skill.ID,
				Path:    rel,
				Content: def.Blobs[rel],
			}); err != nil {
				return nil, fmt.Errorf("starterkit: skill %q file %q: %w", def.Name, rel, err)
			}
			res.SkillFiles++
		}
	}
	return ids, nil
}

func ensureAgents(ctx context.Context, q Querier, workspaceID, owner pgtype.UUID,
	b *Bundle, skillIDs map[string]pgtype.UUID, res *Result) (map[string]pgtype.UUID, error) {

	ids := make(map[string]pgtype.UUID, len(b.Agents))
	for _, def := range b.Agents {
		existing, err := q.GetAgentIDByWorkspaceAndName(ctx, db.GetAgentIDByWorkspaceAndNameParams{
			WorkspaceID: workspaceID,
			Name:        def.Name,
		})
		if err == nil {
			ids[def.Key] = existing
			res.Reused++
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("starterkit: look up agent %q: %w", def.Name, err)
		}

		agent, err := q.CreateAgent(ctx, db.CreateAgentParams{
			WorkspaceID:        workspaceID,
			Name:               def.Name,
			Description:        def.Description,
			AvatarUrl:          optText(def.AvatarURL),
			RuntimeMode:        def.RuntimeMode,
			RuntimeConfig:      []byte("{}"),
			RuntimeID:          pgtype.UUID{},
			Visibility:         def.Visibility,
			MaxConcurrentTasks: def.MaxConcurrentTasks,
			OwnerID:            owner,
			Instructions:       def.Instructions,
			CustomEnv:          []byte("{}"),
			CustomArgs:         []byte("[]"),
			McpConfig:          nil,
			Model:              optText(def.Model),
			PermissionMode:     def.PermissionMode,
		})
		if err != nil {
			return nil, fmt.Errorf("starterkit: create agent %q: %w", def.Name, err)
		}
		ids[def.Key] = agent.ID
		res.Agents++

		for _, name := range def.Skills {
			skillID, ok := skillIDs[name]
			if !ok {
				return nil, fmt.Errorf("starterkit: agent %q binds skill %q, which did not resolve", def.Key, name)
			}
			if err := q.AddAgentSkill(ctx, db.AddAgentSkillParams{
				AgentID: agent.ID,
				SkillID: skillID,
			}); err != nil {
				return nil, fmt.Errorf("starterkit: bind %q to skill %q: %w", def.Name, name, err)
			}
			res.Bindings++
		}
	}
	return ids, nil
}

// ensureSquads imports each squad with its leader and roster. An unresolvable
// leader is an import failure rather than a skip: validate() already proved
// every reference points inside the bundle, so failing here means the import
// itself lost a row it had just created — half a roster is not worth keeping.
func ensureSquads(ctx context.Context, q Querier, workspaceID, owner pgtype.UUID,
	b *Bundle, agentIDs map[string]pgtype.UUID, res *Result) error {

	for _, def := range b.Squads {
		leader, ok := agentIDs[def.Leader]
		if !ok {
			return fmt.Errorf("starterkit: squad %q leader %q did not resolve", def.Name, def.Leader)
		}
		if _, err := q.GetSquadIDByWorkspaceAndName(ctx, db.GetSquadIDByWorkspaceAndNameParams{
			WorkspaceID: workspaceID,
			Name:        def.Name,
		}); err == nil {
			// The name is taken, so the roster is that squad's business — a
			// workspace that trimmed it does not get the bundle's members back.
			res.Reused++
			continue
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("starterkit: look up squad %q: %w", def.Name, err)
		}
		squad, err := q.CreateSquad(ctx, db.CreateSquadParams{
			WorkspaceID: workspaceID,
			Name:        def.Name,
			Description: def.Description,
			LeaderID:    leader,
			CreatorID:   owner,
			AvatarUrl:   optText(def.AvatarURL),
		})
		if err != nil {
			return fmt.Errorf("starterkit: create squad %q: %w", def.Name, err)
		}
		res.Squads++

		if err := addMember(ctx, q, squad.ID, leader, "leader", res); err != nil {
			return err
		}
		for _, m := range def.Members {
			memberID, ok := agentIDs[m.Agent]
			if !ok {
				return fmt.Errorf("starterkit: squad %q member %q did not resolve", def.Name, m.Agent)
			}
			if memberID == leader {
				// The roster lists its leader as a member too; the leader row
				// added above is that row, and squad_member allows one per agent.
				continue
			}
			if err := addMember(ctx, q, squad.ID, memberID, m.Role, res); err != nil {
				return err
			}
		}
	}
	return nil
}

func addMember(ctx context.Context, q Querier, squadID, agentID pgtype.UUID, role string, res *Result) error {
	if _, err := q.AddSquadMember(ctx, db.AddSquadMemberParams{
		SquadID:    squadID,
		MemberType: "agent",
		MemberID:   agentID,
		Role:       role,
	}); err != nil {
		return fmt.Errorf("starterkit: squad member %q (%s): %w", uuidText(agentID), role, err)
	}
	res.SquadMembers++
	return nil
}

// optText maps an empty fixture string to SQL NULL, so a column the bundle does
// not use stays unset instead of carrying a zero-length string the UI then has
// to special-case.
func optText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// uuidText is the log-friendly form of a fixture id. Only ever used for
// diagnostics, never parsed back.
func uuidText(u pgtype.UUID) string { return util.UUIDToString(u) }
