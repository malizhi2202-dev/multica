package starterkit

import (
	"strings"
	"testing"
)

// The fixtures are the product: they ship inside the binary and land in customer
// databases, so they get checked here instead of at first import.

func TestLoadRealBundle(t *testing.T) {
	b, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(b.Skills) == 0 || len(b.Agents) == 0 || len(b.Squads) == 0 {
		t.Fatalf("bundle looks empty: %d skills, %d agents, %d squads",
			len(b.Skills), len(b.Agents), len(b.Squads))
	}

	for _, s := range b.Skills {
		if strings.TrimSpace(s.Body) == "" {
			t.Errorf("skill %q: empty body", s.Name)
		}
		if strings.HasPrefix(s.Body, "---") {
			t.Errorf("skill %q: frontmatter was not stripped", s.Name)
		}
		if s.Description == "" {
			t.Errorf("skill %q: no description outside the file, so the column would be empty", s.Name)
		}
		// Round-trip: the fixture file must be exactly the frontmatter block plus
		// the body we import, with nothing lost or invented between them. A body
		// that begins with a newline would pass the suffix test while quietly
		// differing from what the export read out of the database.
		raw, err := readFixture(s.Dir + "/SKILL.md")
		if err != nil {
			t.Fatalf("skill %q: %v", s.Name, err)
		}
		if !strings.HasSuffix(raw, s.Body) {
			t.Errorf("skill %q: imported body is not the tail of SKILL.md", s.Name)
			continue
		}
		if head := raw[:len(raw)-len(s.Body)]; !strings.HasSuffix(head, "---\n") {
			t.Errorf("skill %q: %q does not end at the frontmatter fence", s.Name, tailOf(head))
		}
		if strings.HasPrefix(s.Body, "\n") {
			t.Errorf("skill %q: body starts with a newline the importer strips", s.Name)
		}
		for _, rel := range s.Files {
			if _, ok := s.Blobs[rel]; !ok {
				t.Errorf("skill %q: kit.json lists file %q, which was not loaded", s.Name, rel)
			}
		}
	}
	// Every declared file and reference has to resolve, which loadBodies proves
	// by failing; these totals are what the DB test compares rows against.
	var files, bindings, members int
	for _, s := range b.Skills {
		files += len(s.Files)
	}
	for _, a := range b.Agents {
		bindings += len(a.Skills)
		if a.Instructions == "" {
			t.Errorf("agent %q: no instructions", a.Key)
		}
		if a.Visibility != "workspace" && a.Visibility != "private" {
			t.Errorf("agent %q: unexpected visibility %q", a.Key, a.Visibility)
		}
	}
	for _, sq := range b.Squads {
		// One leader row plus every roster entry that is not the leader.
		members++
		for _, m := range sq.Members {
			if m.Agent != sq.Leader {
				members++
			}
		}
	}
	t.Logf("bundle: %d skills (+%d files), %d agents (+%d bindings), %d squads (+%d members)",
		len(b.Skills), files, len(b.Agents), bindings, len(b.Squads), members)
}

func TestValidateRejectsBrokenBundles(t *testing.T) {
	good := func() *Bundle {
		return &Bundle{
			Skills: []SkillDef{{Name: "s1", Description: "d", Body: "body",
				Files: []string{"references/a.md"}, Blobs: map[string]string{"references/a.md": "a"}}},
			Agents: []AgentDef{{Key: "a1", Name: "A", Visibility: "private",
				PermissionMode: "private", MaxConcurrentTasks: 6, RuntimeMode: "local",
				Skills: []string{"s1"}, Instructions: "do the thing"}},
			Squads: []SquadDef{{Key: "q1", Name: "Q", Leader: "a1",
				Members: []SquadMemberDef{{Agent: "a1", Role: "leader"}}}},
		}
	}
	if err := good().validate(); err != nil {
		t.Fatalf("the control bundle should validate: %v", err)
	}

	for _, tc := range []struct {
		name string
		poke func(*Bundle)
		want string
	}{
		{"no skills", func(b *Bundle) { b.Skills = nil }, "no skills"},
		{"duplicate skill name", func(b *Bundle) { b.Skills = append(b.Skills, b.Skills[0]) }, "duplicate skill name"},
		{"empty skill body", func(b *Bundle) { b.Skills[0].Body = "  " }, "no body"},
		{"duplicate agent key", func(b *Bundle) {
			b.Agents = append(b.Agents, AgentDef{Key: "a1", Name: "B", Visibility: "private"})
		}, "duplicate agent key"},
		{"duplicate agent name", func(b *Bundle) {
			b.Agents = append(b.Agents, AgentDef{Key: "a2", Name: "A", Visibility: "private"})
		}, "duplicate agent name"},
		{"agent binds unknown skill", func(b *Bundle) { b.Agents[0].Skills = []string{"nope"} }, "unknown skill"},
		{"squad leader unknown", func(b *Bundle) { b.Squads[0].Leader = "ghost" }, "unknown leader"},
		{"squad member unknown", func(b *Bundle) {
			b.Squads[0].Members = []SquadMemberDef{{Agent: "ghost", Role: "member"}}
		}, "unknown member"},
		{"duplicate squad name", func(b *Bundle) { b.Squads = append(b.Squads, b.Squads[0]) }, "duplicate squad name"},
	} {
		b := good()
		tc.poke(b)
		err := b.validate()
		if err == nil {
			t.Errorf("%s: validate accepted a broken bundle", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

func TestSafeRelPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"references/a.md", true},
		{"assets/sub/b.md", true},
		{"", false},
		{"/etc/passwd", false},
		{"../escape.md", false},
		{"references/../escape.md", false},
		{"./a.md", false},
		{"a//b.md", false},
	} {
		if got := safeRelPath(tc.path); got != tc.ok {
			t.Errorf("safeRelPath(%q) = %v, want %v", tc.path, got, tc.ok)
		}
	}
}

func TestStripFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"no frontmatter", "# Title\nbody", "# Title\nbody"},
		{"block then body", "---\nname: x\ndescription: \"y\"\n---\n# Title\n", "# Title\n"},
		{"block only", "---\nname: x\n---", ""},
		{"crlf", "---\r\nname: x\r\n---\r\nbody", "body"},
		{"horizontal rule in body survives", "---\nname: x\n---\n# T\n\n---\n\nmore", "# T\n\n---\n\nmore"},
		{"unterminated block kept whole", "---\nname: x\nno close", "---\nname: x\nno close"},
		{"leading --- inside body untouched", "---\nname: x\n---\nbody", "body"},
	} {
		if got := stripFrontmatter(tc.in); got != tc.want {
			t.Errorf("%s: stripFrontmatter = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func tailOf(s string) string {
	if len(s) <= 12 {
		return s
	}
	return "…" + s[len(s)-12:]
}
