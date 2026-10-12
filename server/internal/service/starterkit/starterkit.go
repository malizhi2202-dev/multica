// Package starterkit imports a bundled set of skills, agents, and squads into a
// workspace, exactly once per workspace.
//
// The bundle lives in fixtures/ and ships inside the server binary, so a fresh
// database — a new worktree, `make destroy`, a colleague's checkout, a self-host
// install — gets a usable set of collaborators without anyone re-importing them
// by hand. That is the whole reason it is code rather than a script.
//
// Two rules keep it from becoming the thing that quietly rewrites a workspace on
// boot:
//
//  1. It runs once. starter_kit_seed holds one claim row per workspace, and the
//     claim plus every insert share a transaction, so a completed import can
//     never run again while a failed one leaves nothing behind.
//  2. It never overwrites. Each definition is matched by the name the product
//     already enforces as workspace-unique (agent, skill, squad); an existing
//     row is reused as-is, with its bindings and files left alone. Rename a
//     kit agent and the kit does not resurrect the old name — it simply stops
//     recognising the row, which is only read during that one import.
//
// Identity is by name, not by agent.system_key: system_key marks a
// product-controlled agent whose prompt the server layers in at task time (see
// internal/service/builtin_agents.go), and kit rows are the opposite — the
// workspace owns them and may edit instructions, skills, and visibility freely.
package starterkit

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// KitKey names this bundle inside starter_kit_seed. A second, unrelated bundle
// would add its own key rather than version this one.
const KitKey = "multica-starter"

// KitVersion records which revision of the bundle landed. It is diagnostics
// only: bumping it does not re-run the import in a workspace already seeded.
const KitVersion = "2026.10"

// fixtureRoot is the embed path prefix for every file in fixtures/.
const fixtureRoot = "fixtures"

//go:embed fixtures
var fixtureFS embed.FS

// Querier is the slice of db.Queries this package uses, so a caller can hand it
// its own transaction — the same shape internal/issuestatus ensures with.
type Querier interface {
	ClaimStarterKitSeed(ctx context.Context, arg db.ClaimStarterKitSeedParams) (int64, error)
	GetStarterKitSeed(ctx context.Context, arg db.GetStarterKitSeedParams) (db.StarterKitSeed, error)
	ListAllWorkspaceIDs(ctx context.Context) ([]pgtype.UUID, error)
	GetWorkspaceOwnerUserID(ctx context.Context, workspaceID pgtype.UUID) (pgtype.UUID, error)
	GetSkillByWorkspaceAndName(ctx context.Context, arg db.GetSkillByWorkspaceAndNameParams) (db.Skill, error)
	GetAgentIDByWorkspaceAndName(ctx context.Context, arg db.GetAgentIDByWorkspaceAndNameParams) (pgtype.UUID, error)
	GetSquadIDByWorkspaceAndName(ctx context.Context, arg db.GetSquadIDByWorkspaceAndNameParams) (pgtype.UUID, error)
	CreateSkill(ctx context.Context, arg db.CreateSkillParams) (db.Skill, error)
	UpsertSkillFile(ctx context.Context, arg db.UpsertSkillFileParams) (db.SkillFile, error)
	CreateAgent(ctx context.Context, arg db.CreateAgentParams) (db.Agent, error)
	AddAgentSkill(ctx context.Context, arg db.AddAgentSkillParams) error
	CreateSquad(ctx context.Context, arg db.CreateSquadParams) (db.Squad, error)
	AddSquadMember(ctx context.Context, arg db.AddSquadMemberParams) (db.SquadMember, error)
}

// SkillDef is one bundled skill. Body and Files are the imported text; the
// frontmatter block in SKILL.md is stripped, so `Description` lives here once
// rather than in two places.
type SkillDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Dir         string   `json:"dir"`
	Files       []string `json:"files"`

	Body  string            `json:"-"`
	Blobs map[string]string `json:"-"`
}

// AgentDef is one bundled agent. Key is a stable ASCII identifier used only for
// fixtures and squad references; the workspace sees Name.
type AgentDef struct {
	Key                string   `json:"key"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	AvatarURL          string   `json:"avatar_url"`
	InstructionsFile   string   `json:"instructions"`
	Skills             []string `json:"skills"`
	Visibility         string   `json:"visibility"`
	PermissionMode     string   `json:"permission_mode"`
	MaxConcurrentTasks int32    `json:"max_concurrent_tasks"`
	RuntimeMode        string   `json:"runtime_mode"`
	Model              string   `json:"model"`

	Instructions string `json:"-"`
}

type SquadMemberDef struct {
	Agent string `json:"agent"`
	Role  string `json:"role"`
}

// SquadDef is one bundled squad. Leader and Members name agents by Key, which
// is why the whole bundle is validated together at load: a typo is a build-time
// fault, not a workspace that silently gets half a roster.
type SquadDef struct {
	Key         string           `json:"key"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	AvatarURL   string           `json:"avatar_url"`
	Leader      string           `json:"leader"`
	Members     []SquadMemberDef `json:"members"`
}

// Bundle is the parsed fixtures tree.
type Bundle struct {
	Skills []SkillDef
	Agents []AgentDef
	Squads []SquadDef
}

// Result reports what one Ensure call did. Reused counts rows that already
// existed by name and were therefore left untouched.
type Result struct {
	Seeded       bool
	Skills       int
	SkillFiles   int
	Agents       int
	Bindings     int
	Squads       int
	SquadMembers int
	Reused       int
}

// bundle parses the embedded fixtures once per process: 200+ files and
// more than a megabyte of text, which no workspace should pay for again.
var bundle = sync.OnceValues(func() (*Bundle, error) {
	raw, err := fixtureFS.ReadFile(path.Join(fixtureRoot, "kit.json"))
	if err != nil {
		return nil, fmt.Errorf("starterkit: read kit.json: %w", err)
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("starterkit: parse kit.json: %w", err)
	}
	if err := b.loadBodies(); err != nil {
		return nil, err
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return &b, nil
})

// Load returns the parsed bundle, so callers (and tests) can inspect it without
// re-reading the embed tree.
func Load() (*Bundle, error) { return bundle() }

func (b *Bundle) loadBodies() error {
	for i := range b.Skills {
		s := &b.Skills[i]
		text, err := readFixture(s.Dir + "/SKILL.md")
		if err != nil {
			return err
		}
		s.Body = stripFrontmatter(text)
		s.Blobs = make(map[string]string, len(s.Files))
		for _, rel := range s.Files {
			if !safeRelPath(rel) {
				return fmt.Errorf("starterkit: skill %q has unsafe file path %q", s.Name, rel)
			}
			blob, err := readFixture(path.Join(s.Dir, rel))
			if err != nil {
				return err
			}
			s.Blobs[rel] = blob
		}
	}
	for i := range b.Agents {
		a := &b.Agents[i]
		text, err := readFixture(a.InstructionsFile)
		if err != nil {
			return err
		}
		a.Instructions = text
	}
	return nil
}

func readFixture(rel string) (string, error) {
	raw, err := fs.ReadFile(fixtureFS, path.Join(fixtureRoot, rel))
	if err != nil {
		return "", fmt.Errorf("starterkit: missing fixture %q: %w", rel, err)
	}
	return string(raw), nil
}

// safeRelPath rejects anything that could escape the bundle directory. Fixtures
// are committed code, so this is a guard against a bad regeneration rather than
// untrusted input.
func safeRelPath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.Contains(p, "..") &&
		path.Clean(p) == p
}

// validate enforces the invariants Ensure depends on, at process start rather
// than as a half-applied import.
func (b *Bundle) validate() error {
	if len(b.Skills) == 0 || len(b.Agents) == 0 {
		return errors.New("starterkit: bundle has no skills or no agents")
	}
	skillNames := map[string]bool{}
	agentNames := map[string]bool{}
	agentKeys := map[string]string{}
	for _, s := range b.Skills {
		if s.Name == "" {
			return errors.New("starterkit: skill with an empty name")
		}
		if skillNames[s.Name] {
			return fmt.Errorf("starterkit: duplicate skill name %q", s.Name)
		}
		skillNames[s.Name] = true
		if strings.TrimSpace(s.Body) == "" {
			return fmt.Errorf("starterkit: skill %q has no body", s.Name)
		}
	}
	for _, a := range b.Agents {
		if a.Key == "" || a.Name == "" {
			return fmt.Errorf("starterkit: agent with an empty key or name (key %q)", a.Key)
		}
		if _, dup := agentKeys[a.Key]; dup {
			return fmt.Errorf("starterkit: duplicate agent key %q", a.Key)
		}
		if agentNames[a.Name] {
			return fmt.Errorf("starterkit: duplicate agent name %q", a.Name)
		}
		agentKeys[a.Key] = a.Name
		agentNames[a.Name] = true
		for _, sk := range a.Skills {
			if !skillNames[sk] {
				return fmt.Errorf("starterkit: agent %q binds unknown skill %q", a.Key, sk)
			}
		}
	}
	squadNames := map[string]bool{}
	for _, sq := range b.Squads {
		if sq.Name == "" {
			return errors.New("starterkit: squad with an empty name")
		}
		if squadNames[sq.Name] {
			return fmt.Errorf("starterkit: duplicate squad name %q", sq.Name)
		}
		squadNames[sq.Name] = true
		if _, ok := agentKeys[sq.Leader]; !ok {
			return fmt.Errorf("starterkit: squad %q has unknown leader %q", sq.Key, sq.Leader)
		}
		for _, m := range sq.Members {
			if _, ok := agentKeys[m.Agent]; !ok {
				return fmt.Errorf("starterkit: squad %q has unknown member %q", sq.Key, m.Agent)
			}
		}
	}
	return nil
}

// stripFrontmatter removes a leading YAML block so the body stored in
// skill.content matches what the product's own skill importer keeps: name and
// description live in their columns, not inside the text.
func stripFrontmatter(text string) string {
	for _, open := range []string{"---\n", "---\r\n"} {
		if !strings.HasPrefix(text, open) {
			continue
		}
		rest := text[len(open):]
		// The closing line may end the file (a block with no body after it) or
		// be followed by the body.
		for _, close := range []string{"\n---\n", "\r\n---\r\n"} {
			if i := strings.Index(rest, close); i >= 0 {
				return strings.TrimLeft(rest[i+len(close):], "\r\n")
			}
		}
		if strings.HasSuffix(rest, "---") || strings.HasSuffix(rest, "---\n") ||
			strings.HasSuffix(rest, "---\r\n") {
			return ""
		}
		// An unterminated block is not a frontmatter we wrote: keep the text
		// whole rather than eat half the body guessing where it ends.
		return text
	}
	return text
}
