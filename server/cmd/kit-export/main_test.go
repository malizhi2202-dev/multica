package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The exporter's whole value is that its output is reviewable as a diff, which is
// only true if the same workspace produces the same bytes every time and the same
// names keep the same keys. These tests pin exactly that, without a database.

func TestCarriesData(t *testing.T) {
	// Unset is empty, so a column nobody populated never blocks an export; anything
	// with content does, because those columns are excluded from the bundle.
	populated := []string{`{"A": "b"}`, `[--flag]`, `0`, `nullish`, `[ ]`, `{"":""}`}
	empty := []string{"", "{}", "[]", "null", "  ", "{}\n"}
	for _, s := range populated {
		if !carriesData([]byte(s)) {
			t.Errorf("carriesData(%q) = false, want true", s)
		}
	}
	for _, s := range empty {
		if carriesData([]byte(s)) {
			t.Errorf("carriesData(%q) = true, want false", s)
		}
	}
}

func TestGuardPortableNamesTheOffendingRow(t *testing.T) {
	agents := []agentRow{
		{Name: "clean"},
		{Name: "leaky", CustomEnv: []byte(`{"OPENAI_API_KEY":"sk-secret"}`)},
	}
	skills := []skillRow{{Name: "fine"}, {Name: "configured", Config: []byte(`{"model":"gpt"}`)}}

	err := guardPortable(agents, skills)
	if err == nil {
		t.Fatal("guardPortable accepted a populated custom_env")
	}
	if !strings.Contains(err.Error(), "leaky") {
		t.Errorf("error should name the agent, got %v", err)
	}
	// Either column is enough on its own, and neither message may promise to strip
	// the value silently.
	if err := guardPortable([]agentRow{{Name: "m", McpConfig: []byte(`{"servers":{}}`)}}, nil); err == nil {
		t.Error("guardPortable accepted a populated mcp_config")
	}
	if err := guardPortable(nil, []skillRow{{Name: "s", Config: []byte(`{"a":1}`)}}); err == nil {
		t.Error("guardPortable accepted a populated skill config")
	}
	if err := guardPortable([]agentRow{{Name: "grant", Composio: []string{"github"}}}, nil); err == nil {
		t.Error("guardPortable accepted a composio allowlist")
	}
	if err := guardPortable([]agentRow{{Name: "ok", CustomArgs: []byte("[]"), CustomEnv: []byte("{}")}},
		[]skillRow{{Name: "ok", Config: []byte("{}")}}); err != nil {
		t.Errorf("guardPortable rejected an all-empty workspace: %v", err)
	}
}

func TestAssignKeysKeepsCommittedAssignments(t *testing.T) {
	keys := nameKeys{
		agentToKey: map[string]string{"研发负责人": "dev-lead"},
		squadToKey: map[string]string{"数据小队": "data-squad"},
	}
	agents := []agentRow{{Name: "研发负责人"}, {Name: "data-steward"}}
	squads := []squadRow{{Name: "数据小队"}}

	if err := assignKeys(keys, agents, squads); err != nil {
		t.Fatalf("assignKeys: %v", err)
	}
	if agents[0].Key != "dev-lead" {
		t.Errorf("existing assignment overridden: got %q, want dev-lead", agents[0].Key)
	}
	if agents[1].Key != "data-steward" {
		t.Errorf("ASCII name derived %q, want the slug data-steward", agents[1].Key)
	}
	if squads[0].Key != "data-squad" {
		t.Errorf("squad key = %q, want data-squad", squads[0].Key)
	}
}

func TestAssignKeysRefusesToInventANonASCIKey(t *testing.T) {
	// A Chinese name with no prior assignment could only get a key by transliteration
	// or a random slug, and either one turns the next import into a new agent sitting
	// beside the old one. The operator has to say what the key is.
	keys := nameKeys{agentToKey: map[string]string{}, squadToKey: map[string]string{}}
	err := assignKeys(keys, []agentRow{{Name: "研发负责人"}}, nil)
	if err == nil {
		t.Fatal("assignKeys derived a key for an unmapped non-ASCII name")
	}
	if !strings.Contains(err.Error(), "研发负责人") || !strings.Contains(err.Error(), "kit.json") {
		t.Errorf("error should name the row and where to set the key, got %v", err)
	}
}

func TestSlug(t *testing.T) {
	for name, want := range map[string]string{
		"data-steward":   "data-steward",
		"Data Steward":   "data-steward",
		"QA  Verifier!!": "qa-verifier",
		"---":            "",
		"研发":             "",
		"emoji:🚀 name":   "",
	} {
		got, ok := slug(name)
		if want == "" {
			if ok {
				t.Errorf("slug(%q) = %q, want no key derived (not ASCII, or nothing left to use)", name, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("slug(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
}

func TestGuardReferencesCatchesDanglingSquadRows(t *testing.T) {
	keys := nameKeys{agentToKey: map[string]string{"A": "a", "B": "b"}}
	agents := []agentRow{{Name: "A", Key: "a"}, {Name: "B", Key: "b"}}
	ok := []squadRow{{Name: "s", Leader: "A", Members: []squadMemberRow{{Agent: "B"}}}}
	if err := guardReferences(keys, agents, ok); err != nil {
		t.Fatalf("valid squad rejected: %v", err)
	}

	// A squad whose leader is an agent in the workspace but not in this export: the
	// import would resolve the reference to a key nothing defines.
	missing := []squadRow{{Name: "s", Leader: "C"}}
	if err := guardReferences(keys, agents, missing); err == nil {
		t.Error("squad led by an unexported agent accepted")
	}
	dupKey := []agentRow{{Name: "A", Key: "a"}, {Name: "A2", Key: "a"}}
	if err := guardReferences(keys, dupKey, nil); err == nil {
		t.Error("two agents sharing one fixture key accepted")
	}
}

func TestValidateRelPath(t *testing.T) {
	for _, bad := range []string{"", "/abs/path.md", "..", "../outside.md", "a/./b.md", "a//b.md", "a/../b.md"} {
		if err := validateRelPath(bad); err == nil {
			t.Errorf("validateRelPath(%q) accepted a path that does not stay inside a skill directory", bad)
		}
	}
	for _, good := range []string{"references/a.md", "assets/sub/file.json", "a.md"} {
		if err := validateRelPath(good); err != nil {
			t.Errorf("validateRelPath(%q) = %v", good, err)
		}
	}
}

func TestYAMLFrontmatterForm(t *testing.T) {
	// Names stay bare, descriptions stay quoted: that is the shape the committed
	// fixtures have, so a regeneration does not touch 30 files it has nothing to say
	// about.
	if got := yamlPlain("code-review"); got != "code-review" {
		t.Errorf("yamlPlain(plain name) = %q, want it unquoted", got)
	}
	if got := yamlQuoted("架构：设计"); got != `"架构：设计"` {
		t.Errorf("yamlQuoted = %q", got)
	}
	for _, tricky := range []string{`has "quotes"`, "two\nlines", `back\slash`} {
		got := yamlPlain(tricky)
		if !strings.HasPrefix(got, `"`) || strings.Contains(got, "\n") {
			t.Errorf("yamlPlain(%q) = %q, want an escaped single-line scalar", tricky, got)
		}
	}
	// A value that would be re-read differently unquoted must be quoted.
	if got := yamlPlain("name: with colon"); got == "name: with colon" {
		t.Error("yamlPlain left an embedded colon-space unquoted, which re-parses as a nested mapping")
	}
}

func TestRenderIsDeterministicAndShapedLikeTheCommittedKit(t *testing.T) {
	keys := nameKeys{
		agentToKey: map[string]string{"研发负责人": "dev-lead", "测试": "qa"},
		squadToKey: map[string]string{"小队": "squad"},
	}
	skills := []skillRow{{
		Name: "code-review", Description: "评审。", Content: "# body\n",
		Files: []skillFile{{Path: "references/z.md", Content: "z"}, {Path: "assets/a.md", Content: "a"}},
	}}
	agents := []agentRow{{
		Name: "研发负责人", Key: "dev-lead", Description: "负责。", Instructions: "做实现",
		Visibility: "workspace", PermissionMode: "public_to", RuntimeMode: "local",
		MaxConcurrentTasks: 6, Skills: []string{"code-review"},
	}}
	squads := []squadRow{{
		Name: "小队", Key: "squad", Leader: "研发负责人",
		Members: []squadMemberRow{{Agent: "研发负责人", Role: "leader"}, {Agent: "测试", Role: "member"}},
	}}

	first, err := render(keys, skills, agents, squads)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := render(keys, skills, agents, squads)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("render is not deterministic; the export would produce a churn diff")
	}

	want := map[string]bool{
		"kit.json":                           true,
		"skills/code-review/SKILL.md":        true,
		"skills/code-review/assets/a.md":     true,
		"skills/code-review/references/z.md": true,
		"agents/dev-lead.md":                 true,
	}
	for path := range want {
		if _, ok := first[path]; !ok {
			t.Errorf("render produced no %q (keys: %s)", path, strings.Join(keysOf(first), " "))
		}
	}
	if got := string(first["skills/code-review/SKILL.md"]); !strings.HasPrefix(got, "---\nname: code-review\ndescription: \"评审。\"\n---\n# body\n") {
		t.Errorf("SKILL.md frontmatter changed shape, which the importer's round-trip test depends on:\n%q", got)
	}

	kit := string(first["kit.json"])
	if !strings.HasSuffix(kit, "}\n") {
		t.Error("kit.json must end with a newline")
	}
	if strings.Contains(kit, "\\u") {
		t.Error("kit.json escaped non-ASCII to \\u sequences; the committed bundle is raw UTF-8")
	}
	if !strings.Contains(kit, "\n      \"files\": [\n") {
		t.Errorf("kit.json is not indented two spaces per level:\n%s", firstTwoHundred(kit))
	}
	// Sorted keys is what makes the map-based encoding match the committed file.
	if i := strings.Index(kit, "\"agents\""); i == -1 || strings.Index(kit, "\"skills\"") < i {
		t.Error("kit.json keys are not in sorted order")
	}
}

func TestRenderRejectsAnEscapingSkillFilePath(t *testing.T) {
	keys := nameKeys{agentToKey: map[string]string{}, squadToKey: map[string]string{}}
	skills := []skillRow{{Name: "s", Description: "d", Content: "c",
		Files: []skillFile{{Path: "../escape.md", Content: "x"}}}}
	if _, err := render(keys, skills, nil, nil); err == nil {
		t.Fatal("render wrote a skill file outside its directory")
	}
}

func TestCheckDriftReportsEachKindOfDifference(t *testing.T) {
	dir := t.TempDir()
	keys := nameKeys{agentToKey: map[string]string{"A": "a"}, squadToKey: map[string]string{}}
	agents := []agentRow{{Name: "A", Key: "a", Instructions: "x", Visibility: "workspace",
		PermissionMode: "private", RuntimeMode: "local"}}
	skills := []skillRow{{Name: "s", Description: "d", Content: "body",
		Files: []skillFile{{Path: "references/gone.md", Content: "y"}}}}

	// Nothing on disk yet: the export is one big "missing", and a first run is not
	// reported as drift in the stale direction because there is no tree to be stale.
	if err := checkDrift(dir, keys, skills, agents, nil); err == nil {
		t.Fatal("checkDrift reported a sync against an empty directory")
	}
	if err := writeFixtures(dir, keys, skills, agents, nil); err != nil {
		t.Fatalf("writeFixtures: %v", err)
	}
	if err := checkDrift(dir, keys, skills, agents, nil); err != nil {
		t.Fatalf("checkDrift after writing the same bundle: %v", err)
	}

	// A content change in one file, and a file the workspace no longer has.
	if err := os.WriteFile(filepath.Join(dir, "kit.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkDrift(dir, keys, skills, agents, nil); err == nil {
		t.Error("checkDrift accepted a stale kit.json")
	}
	short := []skillRow{{Name: "s", Description: "d", Content: "body"}}
	if err := os.WriteFile(filepath.Join(dir, "kit.json"), mustRender(t, keys, short, agents, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkDrift(dir, keys, short, agents, nil); err == nil {
		t.Error("checkDrift accepted a tree with a file the bundle no longer produces")
	} else if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error should have listed the orphaned reference file, got %v", err)
	}
}

func TestEmbeddedKeysCoverTheCommittedBundle(t *testing.T) {
	// The fallback that lets an export into a fresh directory stay stable: every
	// name in the shipped bundle must resolve to the key the bundle already uses,
	// or regeneration would re-key the kit.
	keys, err := embeddedKeys("does-not-exist")
	if err != nil {
		t.Skipf("no embedded bundle to inherit keys from: %v", err)
	}
	if len(keys.agentToKey) == 0 || len(keys.squadToKey) == 0 {
		t.Fatalf("embedded keys empty: %d agents, %d squads", len(keys.agentToKey), len(keys.squadToKey))
	}
	seen := map[string]bool{}
	for name, key := range keys.agentToKey {
		if seen[key] {
			t.Errorf("two bundle agents share key %q (%q and another)", key, name)
		}
		seen[key] = true
		if _, ok := slug(name); !ok && key == "" {
			t.Errorf("agent %q has no key", name)
		}
	}
}

func TestKitExportAgainstARealWorkspace(t *testing.T) {
	// The round trip that matters: a workspace's current rows regenerate exactly the
	// bundle that was imported from it, which is also the case that keeps the
	// importer's name matching honest. Opt in because it reads a live workspace:
	//
	//   KIT_EXPORT_WORKSPACE=my-slug go test ./cmd/kit-export -run TestKitExport -v
	workspace := os.Getenv("KIT_EXPORT_WORKSPACE")
	if workspace == "" {
		t.Skip("set KIT_EXPORT_WORKSPACE to a workspace slug or id to compare the bundle against it")
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	if _, err := os.Stat(filepath.Join("..", "..", "internal", "service", "starterkit", "fixtures")); err != nil {
		t.Skipf("fixtures not found relative to this test binary: %v", err)
	}
	if err := run(dbURL, workspace, filepath.Join("..", "..", "internal", "service", "starterkit", "fixtures"), true); err != nil {
		t.Errorf("committed fixtures drifted from workspace %q: %v", workspace, err)
	}
}

// helpers ------------------------------------------------------------

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustRender(t *testing.T, keys nameKeys, skills []skillRow, agents []agentRow, squads []squadRow) []byte {
	t.Helper()
	files, err := render(keys, skills, agents, squads)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return files["kit.json"]
}

func firstTwoHundred(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
