// kit-export regenerates the starter kit fixtures embedded by
// server/internal/service/starterkit, from a workspace that already holds the
// skills, agents, and squads a deployment wants to hand to new workspaces.
//
// It exists so the bundle can be refreshed after the real work drifts — a skill
// edited in the product, an agent's instructions rewritten, a squad re-rostered —
// without re-assembling 200 files by hand and hoping the names still match:
//
//	go run ./cmd/kit-export -workspace my-slug          # rewrite the fixtures
//	go run ./cmd/kit-export -workspace my-slug -check   # report drift, write nothing
//	git diff --stat internal/service/starterkit/fixtures
//
// The output is reviewed source, so it stays byte-stable across runs: rows are
// sorted by name, file lists by path, and kit.json keys are sorted the way the
// committed file is now formatted. That makes `git diff` the whole review.
//
// What it deliberately does not carry across:
//
//   - Any id. Skills, agents, and squads are matched by name when importing (agent
//     and skill both have UNIQUE(workspace_id, name)), so regenerating stays stable
//     instead of minting identifiers nobody can map back.
//   - runtime_id. A runtime is one workspace's infrastructure; an agent shipped in a
//     bundle belongs to no machine until the importing workspace says so.
//   - custom_env, custom_args, mcp_config, composio_toolkit_allowlist, and skill
//     config. These hold credentials and machine-local settings, so the export stops
//     if any is populated rather than quietly dropping the difference.
//   - owner_id, created_by, creator_id. Resolved to the importing workspace's owner
//     at seed time.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/service/starterkit"
)

func main() {
	dbURL := flag.String("database-url", os.Getenv("DATABASE_URL"),
		"postgres connection string; defaults to $DATABASE_URL")
	workspace := flag.String("workspace", "",
		"workspace slug or id to export from (required)")
	fixtures := flag.String("fixtures", "internal/service/starterkit/fixtures",
		"fixtures directory to rewrite (relative to the server module root)")
	check := flag.Bool("check", false,
		"report whether the fixtures match the workspace and exit non-zero on drift, writing nothing")
	flag.Parse()

	if *workspace == "" {
		fmt.Fprintln(os.Stderr, "kit-export: -workspace is required (slug or id)")
		os.Exit(2)
	}
	if *dbURL == "" {
		fmt.Fprintln(os.Stderr, "kit-export: -database-url or DATABASE_URL is required")
		os.Exit(2)
	}
	if err := run(*dbURL, *workspace, *fixtures, *check); err != nil {
		fmt.Fprintf(os.Stderr, "kit-export: %v\n", err)
		os.Exit(1)
	}
}

func run(dbURL, workspace, fixturesDir string, check bool) error {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	var workspaceID string
	err = pool.QueryRow(ctx,
		`SELECT id::text FROM workspace WHERE slug = $1 OR id::text = $1::text`, workspace).
		Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("no workspace with slug or id %q", workspace)
	}
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	// Names to fixture keys comes from the committed bundle, not a table in this
	// file: kit.json already records which key stands for which agent, so
	// regenerating cannot re-key an agent that the last commit settled on.
	keys, err := loadKeys(fixturesDir)
	if err != nil {
		return err
	}

	skills, err := readSkills(ctx, pool, workspaceID)
	if err != nil {
		return err
	}
	agents, err := readAgents(ctx, pool, workspaceID)
	if err != nil {
		return err
	}
	squads, err := readSquads(ctx, pool, workspaceID)
	if err != nil {
		return err
	}
	if err := guardPortable(agents, skills); err != nil {
		return err
	}
	if err := assignKeys(keys, agents, squads); err != nil {
		return err
	}
	if err := guardReferences(keys, agents, squads); err != nil {
		return err
	}

	if check {
		return checkDrift(fixturesDir, keys, skills, agents, squads)
	}
	return writeFixtures(fixturesDir, keys, skills, agents, squads)
}

type skillFile struct {
	Path    string
	Content string
}

type skillRow struct {
	Name        string
	Description string
	Content     string
	Config      []byte
	Files       []skillFile
}

type agentRow struct {
	Name               string
	Description        string
	AvatarURL          string
	Instructions       string
	Visibility         string
	PermissionMode     string
	RuntimeMode        string
	Model              string
	MaxConcurrentTasks int32
	Skills             []string
	CustomEnv          []byte
	CustomArgs         []byte
	McpConfig          []byte
	Composio           []string
	Key                string
}

type squadMemberRow struct {
	Agent string
	Role  string
}

type squadRow struct {
	Name        string
	Description string
	AvatarURL   string
	Leader      string
	Members     []squadMemberRow
	Key         string
}

// nameKeys resolves display names to fixture keys in both directions.
type nameKeys struct {
	agentToKey map[string]string
	squadToKey map[string]string
}

func (k nameKeys) agent(name string) (string, bool) {
	key, ok := k.agentToKey[name]
	return key, ok
}

func (k nameKeys) hasAgent(key string) bool {
	for _, v := range k.agentToKey {
		if v == key {
			return true
		}
	}
	return false
}

// loadKeys reads the name-to-key assignments the bundle already carries, so a
// regeneration can never re-key a row the last commit settled on. A brand-new name
// falls through to assignKeys, which derives a key only where that is safe.
func loadKeys(fixturesDir string) (nameKeys, error) {
	keys := nameKeys{
		agentToKey: map[string]string{},
		squadToKey: map[string]string{},
	}
	raw, err := os.ReadFile(filepath.Join(fixturesDir, "kit.json"))
	if errors.Is(err, os.ErrNotExist) {
		// The target has no bundle yet — an export into a fresh directory. The
		// mapping still exists: it is embedded in this binary. Falling back to it
		// means regenerating somewhere else cannot re-key an agent that the
		// committed fixtures already settled on, which is the one thing a stable
		// export must never do.
		return embeddedKeys(fixturesDir)
	}
	if err != nil {
		return keys, fmt.Errorf("read %s/kit.json: %w", fixturesDir, err)
	}
	var kit struct {
		Agents []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"agents"`
		Squads []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"squads"`
	}
	if err := json.Unmarshal(raw, &kit); err != nil {
		return keys, fmt.Errorf("parse %s/kit.json: %w", fixturesDir, err)
	}
	for _, a := range kit.Agents {
		keys.agentToKey[a.Name] = a.Key
	}
	for _, s := range kit.Squads {
		keys.squadToKey[s.Name] = s.Key
	}
	return keys, nil
}

// embeddedKeys reads the same assignments out of the bundle this binary carries.
func embeddedKeys(fixturesDir string) (nameKeys, error) {
	keys := nameKeys{
		agentToKey: map[string]string{},
		squadToKey: map[string]string{},
	}
	bundle, err := starterkit.Load()
	if err != nil {
		return keys, fmt.Errorf("no kit.json in %s to inherit key assignments from, and the bundle "+
			"embedded in this binary cannot be read either: %w", fixturesDir, err)
	}
	for _, a := range bundle.Agents {
		keys.agentToKey[a.Name] = a.Key
	}
	for _, s := range bundle.Squads {
		keys.squadToKey[s.Name] = s.Key
	}
	return keys, nil
}

func readSkills(ctx context.Context, pool *pgxpool.Pool, ws string) ([]skillRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT name, description, content, config
		FROM skill
		WHERE workspace_id = $1
		ORDER BY name`, ws)
	if err != nil {
		return nil, fmt.Errorf("read skills: %w", err)
	}
	defer rows.Close()

	out := []skillRow{}
	for rows.Next() {
		var s skillRow
		if err := rows.Scan(&s.Name, &s.Description, &s.Content, &s.Config); err != nil {
			return nil, fmt.Errorf("read skills: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read skills: %w", err)
	}

	files, err := pool.Query(ctx, `
		SELECT s.name, f.path, f.content
		FROM skill_file f
		JOIN skill s ON s.id = f.skill_id
		WHERE s.workspace_id = $1
		ORDER BY s.name, f.path`, ws)
	if err != nil {
		return nil, fmt.Errorf("read skill files: %w", err)
	}
	defer files.Close()

	index := map[string]int{}
	for i, s := range out {
		index[s.Name] = i
	}
	for files.Next() {
		var skill, path, content string
		if err := files.Scan(&skill, &path, &content); err != nil {
			return nil, fmt.Errorf("read skill files: %w", err)
		}
		if strings.EqualFold(path, "SKILL.md") {
			return nil, fmt.Errorf("skill %q has a file at %q, which is the entry point's own path",
				skill, path)
		}
		s := out[index[skill]]
		s.Files = append(s.Files, skillFile{Path: path, Content: content})
		out[index[skill]] = s
	}
	if err := files.Err(); err != nil {
		return nil, fmt.Errorf("read skill files: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("workspace has no skills to bundle")
	}
	return out, nil
}

func readAgents(ctx context.Context, pool *pgxpool.Pool, ws string) ([]agentRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT name, description, coalesce(avatar_url, ''), instructions, visibility,
		       permission_mode, runtime_mode, coalesce(model, ''), max_concurrent_tasks,
		       custom_env, custom_args, mcp_config, composio_toolkit_allowlist
		FROM agent
		WHERE workspace_id = $1 AND kind = 'user'
		ORDER BY name`, ws)
	if err != nil {
		return nil, fmt.Errorf("read agents: %w", err)
	}
	defer rows.Close()

	out := []agentRow{}
	for rows.Next() {
		var a agentRow
		if err := rows.Scan(&a.Name, &a.Description, &a.AvatarURL, &a.Instructions,
			&a.Visibility, &a.PermissionMode, &a.RuntimeMode, &a.Model,
			&a.MaxConcurrentTasks, &a.CustomEnv, &a.CustomArgs, &a.McpConfig, &a.Composio); err != nil {
			return nil, fmt.Errorf("read agents: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read agents: %w", err)
	}

	// Enabled bindings only. A disabled one is this workspace's runtime choice, not
	// part of what a bundle should hand over.
	bindings, err := pool.Query(ctx, `
		SELECT a.name, s.name
		FROM agent_skill asn
		JOIN agent a ON a.id = asn.agent_id
		JOIN skill s ON s.id = asn.skill_id
		WHERE a.workspace_id = $1 AND a.kind = 'user' AND asn.enabled
		ORDER BY a.name, s.name`, ws)
	if err != nil {
		return nil, fmt.Errorf("read agent skills: %w", err)
	}
	defer bindings.Close()

	index := map[string]int{}
	for i, a := range out {
		index[a.Name] = i
	}
	for bindings.Next() {
		var agent, skill string
		if err := bindings.Scan(&agent, &skill); err != nil {
			return nil, fmt.Errorf("read agent skills: %w", err)
		}
		a := out[index[agent]]
		a.Skills = append(a.Skills, skill)
		out[index[agent]] = a
	}
	if err := bindings.Err(); err != nil {
		return nil, fmt.Errorf("read agent skills: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("workspace has no user agents to bundle")
	}
	return out, nil
}

func readSquads(ctx context.Context, pool *pgxpool.Pool, ws string) ([]squadRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT s.name, s.description, coalesce(s.avatar_url, ''),
		       coalesce((SELECT a.name FROM agent a WHERE a.id = s.leader_id), ''),
		       coalesce((
		           SELECT json_agg(json_build_object('agent', m.agent, 'role', m.role)
		                          ORDER BY m.created_at)
		           FROM (
		               SELECT a2.name AS agent, sm.role, sm.created_at
		               FROM squad_member sm
		               JOIN agent a2 ON a2.id = sm.member_id
		               WHERE sm.squad_id = s.id AND sm.member_type = 'agent'
		           ) m
		       ), '[]')
		FROM squad s
		WHERE s.workspace_id = $1
		ORDER BY s.name`, ws)
	if err != nil {
		return nil, fmt.Errorf("read squads: %w", err)
	}
	defer rows.Close()

	out := []squadRow{}
	for rows.Next() {
		var (
			name, description, avatar, leader string
			members                           []byte
		)
		if err := rows.Scan(&name, &description, &avatar, &leader, &members); err != nil {
			return nil, fmt.Errorf("read squads: %w", err)
		}
		var roster []squadMemberRow
		if err := json.Unmarshal(members, &roster); err != nil {
			return nil, fmt.Errorf("read squads: parse roster of %q: %w", name, err)
		}
		if leader == "" {
			return nil, fmt.Errorf("squad %q has no leader agent; a bundled squad needs one", name)
		}
		out = append(out, squadRow{Name: name, Description: description,
			AvatarURL: avatar, Leader: leader, Members: roster})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read squads: %w", err)
	}
	return out, nil
}

// carriesData reports whether a JSON column holds something other than an empty
// container. NULL scans as a nil slice, so unset, {}, and [] all read as empty:
// they carry nothing that could leak or fail to travel.
func carriesData(raw []byte) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "{}", "[]", "null":
		return false
	default:
		return true
	}
}

// guardPortable stops an export whose source workspace has configuration the
// bundle cannot carry. Excluding those columns silently would let an operator
// believe the workspace was exported in full, so a populated one is an error and
// the operator decides: move the value into the workspace after import, or keep it
// out of the export's reach.
func guardPortable(agents []agentRow, skills []skillRow) error {
	for _, a := range agents {
		for column, raw := range map[string][]byte{
			"custom_env":  a.CustomEnv,
			"custom_args": a.CustomArgs,
			"mcp_config":  a.McpConfig,
		} {
			if carriesData(raw) {
				return fmt.Errorf("agent %q has %s populated; credentials and machine-local "+
					"settings never go into the bundle", a.Name, column)
			}
		}
		if len(a.Composio) > 0 {
			return fmt.Errorf("agent %q has a composio_toolkit_allowlist; third-party tool grants "+
				"are one workspace's decision, not bundle content", a.Name)
		}
	}
	for _, s := range skills {
		if carriesData(s.Config) {
			return fmt.Errorf("skill %q has config populated; the bundle ships an empty config and "+
				"leaves the importing workspace to fill it in", s.Name)
		}
	}
	return nil
}

var asciiSlugable = regexp.MustCompile(`^[[:ascii:]]+$`)

// assignKeys gives every exported row its fixture key. An assignment the bundle
// already carries always wins, so regenerating never re-keys a committed agent. A new
// name with an ASCII display name is slugged; anything else stops, because
// inventing a key for a non-ASCII name is how a rename sneaks into the bundle: the
// import matches on name, so a new key alongside the old one reads as the same
// agent under two identities.
func assignKeys(keys nameKeys, agents []agentRow, squads []squadRow) error {
	unnamed := []string{}
	for i := range agents {
		name := agents[i].Name
		if key, ok := keys.agent(name); ok {
			agents[i].Key = key
			continue
		}
		key, ok := slug(name)
		if !ok {
			unnamed = append(unnamed, name)
			continue
		}
		agents[i].Key = key
		keys.agentToKey[name] = key
	}
	if len(unnamed) > 0 {
		sort.Strings(unnamed)
		return fmt.Errorf("these agents have no key in %s and their names are not ASCII, so no key "+
			"can be derived: %s. Set \"key\" for them in kit.json first — squad references and later "+
			"renames depend on it", "kit.json", strings.Join(unnamed, ", "))
	}
	for i := range squads {
		name := squads[i].Name
		if key, ok := keys.squadToKey[name]; ok {
			squads[i].Key = key
			continue
		}
		key, ok := slug(name)
		if !ok {
			return fmt.Errorf("squad %q has no key in kit.json and its name is not ASCII; set \"key\" "+
				"for it first", name)
		}
		squads[i].Key = key
		keys.squadToKey[name] = key
	}
	return nil
}

func slug(name string) (string, bool) {
	if !asciiSlugable.MatchString(name) {
		return "", false
	}
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			return r
		default:
			return '-'
		}
	}, name)
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	out = strings.Trim(out, "-")
	if out == "" {
		return "", false
	}
	return out, true
}

// guardReferences checks that a squad only points at agents that are being
// exported, so a bundle can never reference an agent by a key nothing defines.
func guardReferences(keys nameKeys, agents []agentRow, squads []squadRow) error {
	exported := map[string]bool{}
	for _, a := range agents {
		if exported[a.Key] {
			return fmt.Errorf("two agents share fixture key %q; give one of them a distinct \"key\" "+
				"in kit.json", a.Key)
		}
		exported[a.Key] = true
	}
	for _, s := range squads {
		leader, ok := keys.agent(s.Leader)
		if !ok || !exported[leader] {
			return fmt.Errorf("squad %q is led by %q, which is not being exported", s.Name, s.Leader)
		}
		for _, m := range s.Members {
			key, ok := keys.agent(m.Agent)
			if !ok || !exported[key] {
				return fmt.Errorf("squad %q lists agent %q, which is not being exported", s.Name, m.Agent)
			}
		}
	}
	return nil
}

// render produces the fixture tree in memory as a deterministic map of relative
// path to content, so writing and checking share one definition of what the
// bundle is.
func render(keys nameKeys, skills []skillRow, agents []agentRow, squads []squadRow) (map[string][]byte, error) {
	out := map[string][]byte{}

	skillEntries := []map[string]any{}
	for _, s := range skills {
		paths := make([]string, 0, len(s.Files))
		for _, f := range s.Files {
			if err := validateRelPath(f.Path); err != nil {
				return nil, err
			}
			path := path.Join(append([]string{"skills", s.Name}, strings.Split(f.Path, "/")...)...)
			if _, clash := out[path]; clash {
				return nil, fmt.Errorf("skill %q has two files at %q", s.Name, f.Path)
			}
			out[path] = []byte(f.Content)
			paths = append(paths, f.Path)
		}
		sort.Strings(paths)
		entry := path.Join("skills", s.Name)
		// The frontmatter block makes each directory read as an ordinary skill that
		// any agent can pick up outside Multica too; the importer strips it again and
		// keeps skill.content and skill.description as the source of truth.
		out[path.Join(entry, "SKILL.md")] = []byte(fmt.Sprintf(
			"---\nname: %s\ndescription: %s\n---\n%s",
			yamlPlain(s.Name), yamlQuoted(s.Description), s.Content))
		skillEntries = append(skillEntries, map[string]any{
			"name": s.Name, "description": s.Description,
			"dir": entry, "files": paths,
		})
	}

	agentEntries := []map[string]any{}
	for _, a := range agents {
		out[path.Join("agents", a.Key+".md")] = []byte(a.Instructions)
		skills := a.Skills
		if skills == nil {
			skills = []string{}
		}
		agentEntries = append(agentEntries, map[string]any{
			"key": a.Key, "name": a.Name, "description": a.Description,
			"avatar_url": a.AvatarURL, "instructions": path.Join("agents", a.Key+".md"),
			"skills": skills, "visibility": a.Visibility, "permission_mode": a.PermissionMode,
			"max_concurrent_tasks": a.MaxConcurrentTasks, "runtime_mode": a.RuntimeMode,
			"model": a.Model,
		})
	}

	squadEntries := []map[string]any{}
	for _, s := range squads {
		members := []map[string]any{}
		for _, m := range s.Members {
			key, _ := keys.agent(m.Agent)
			members = append(members, map[string]any{"agent": key, "role": m.Role})
		}
		leader, _ := keys.agent(s.Leader)
		squadEntries = append(squadEntries, map[string]any{
			"key": s.Key, "name": s.Name, "description": s.Description,
			"avatar_url": s.AvatarURL, "leader": leader, "members": members,
		})
	}

	kit, err := marshalSorted(map[string]any{
		"skills": skillEntries, "agents": agentEntries, "squads": squadEntries,
	})
	if err != nil {
		return nil, err
	}
	out["kit.json"] = kit
	return out, nil
}

// marshalSorted renders JSON the way the committed kit.json is formatted: two
// spaces of indent, object keys alphabetical (which encoding/json does for maps),
// and UTF-8 left as-is rather than escaped to \u sequences. SetEscapeHTML(false)
// keeps & < > literal, so a skill body containing markup survives the round trip.
func marshalSorted(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode kit.json: %w", err)
	}
	return []byte(buf.String()), nil
}

// yamlPlain renders a frontmatter value unquoted when that is safe, matching the
// committed files: a skill name is an ASCII slug, so it stays bare. A value a YAML
// parser would re-read differently — a quote, a newline, an embedded ": ", a leading
// indicator — is quoted and escaped instead, so an unusual name cannot silently
// re-shape the block.
func yamlPlain(s string) string {
	if yamlBareOK.MatchString(s) && !strings.Contains(s, ": ") {
		return s
	}
	return yamlQuoted(s)
}

// yamlQuoted double-quotes a frontmatter value and escapes what needs escaping.
func yamlQuoted(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// yamlBareOK is a YAML plain scalar that survives a re-parse unchanged: an ASCII
// word character to start, then word characters, dots, dashes, underscores, and
// spaces.
var yamlBareOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_. -]*$`)

// validateRelPath keeps every exported file inside its skill directory. The paths
// come from a database this operator controls, so this guards a bad row, not an
// attacker.
func validateRelPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return fmt.Errorf("skill file path %q is not relative", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("skill file path %q escapes its skill directory", p)
		}
	}
	return nil
}

func writeFixtures(fixturesDir string, keys nameKeys, skills []skillRow, agents []agentRow, squads []squadRow) error {
	files, err := render(keys, skills, agents, squads)
	if err != nil {
		return err
	}
	// Rebuild the two generated subtrees from scratch: a file the workspace no
	// longer has must disappear, and only the fixture tree is regenerated, never
	// the directory that holds the Go code beside it.
	for _, subtree := range []string{"skills", "agents"} {
		if err := os.RemoveAll(filepath.Join(fixturesDir, subtree)); err != nil {
			return fmt.Errorf("clear existing %s/: %w", subtree, err)
		}
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		full := filepath.FromSlash(path.Join(fixturesDir, rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create directory for %s: %w", rel, err)
		}
		if err := os.WriteFile(full, files[rel], 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	fmt.Printf("wrote %d files to %s: %d skills, %d agents, %d squads\n",
		len(paths), fixturesDir, len(skills), len(agents), len(squads))
	fmt.Println("Review the diff before committing: the import matches on name, so a rename " +
		"lands as a new row alongside the one it replaces.")
	return nil
}

// checkDrift renders the bundle without touching the tree and reports the files
// that differ, are missing, or are no longer produced — the question an operator
// has before regenerating, and the one CI can ask later.
func checkDrift(fixturesDir string, keys nameKeys, skills []skillRow, agents []agentRow, squads []squadRow) error {
	files, err := render(keys, skills, agents, squads)
	if err != nil {
		return err
	}
	problems := []string{}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.FromSlash(path.Join(fixturesDir, rel)))
		switch {
		case errors.Is(err, os.ErrNotExist):
			problems = append(problems, "missing "+rel)
		case err != nil:
			return fmt.Errorf("read %s: %w", rel, err)
		case string(got) != string(want):
			problems = append(problems, "differs  "+rel)
		}
	}
	// A subtree that does not exist yet is the first export, not stale content, so
	// stale detection only runs once both generated subtrees are on disk.
	onDisk := map[string]bool{}
	for _, subtree := range []string{"skills", "agents"} {
		root := filepath.Join(fixturesDir, subtree)
		if _, err := os.ReadDir(root); errors.Is(err, fs.ErrNotExist) {
			onDisk = nil
			break
		} else if err != nil {
			return fmt.Errorf("read %s: %w", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(fixturesDir, path)
			if err != nil {
				return err
			}
			onDisk[filepath.ToSlash(rel)] = true
			return nil
		})
		if err != nil {
			return fmt.Errorf("walk %s: %w", root, err)
		}
	}
	for rel := range onDisk {
		if _, ok := files[rel]; !ok {
			problems = append(problems, "stale    "+rel)
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println(p)
		}
		// The list goes to stdout for a human running this by hand; the error names
		// the head of it so the same answer survives being wrapped in CI output.
		return fmt.Errorf("%d file(s) out of sync with workspace (first: %s); rerun without -check to regenerate",
			len(problems), strings.Join(firstOf(problems, 3), ", "))
	}
	fmt.Printf("%s is in sync: %d skills, %d agents, %d squads\n",
		fixturesDir, len(skills), len(agents), len(squads))
	return nil
}

// firstOf returns at most n leading entries of s.
func firstOf(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
