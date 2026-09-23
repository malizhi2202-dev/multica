package handler

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// localDirsResponse mirrors the GET /api/workspaces/{id}/local-dirs contract
// field-for-field — the web client parses the same names.
type localDirsResponse struct {
	Path         string `json:"path"`
	Parent       string `json:"parent"`
	Hostname     string `json:"hostname"`
	Home         string `json:"home"`
	DaemonID     string `json:"daemon_id"`
	DaemonStatus string `json:"daemon_status"`
	Dirs         []struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		HasChildren bool   `json:"has_children"`
		Blocked     bool   `json:"blocked"`
	} `json:"dirs"`
}

// blacklistedSystemRoot picks a path protected system root per the shared
// localpath blacklist. The handler rejects it BEFORE touching the filesystem,
// so existence on the test host is irrelevant.
func blacklistedSystemRoot() string {
	if runtime.GOOS == "windows" {
		return `C:\Users`
	}
	return "/etc"
}

func getLocalDirs(t *testing.T, rawQuery string) *testutil.Response {
	t.Helper()
	path := "/api/workspaces/" + testWorkspaceID + "/local-dirs"
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	req := newRequest("GET", path, nil)
	return testutil.Call(t, testHandler.ListWorkspaceLocalDirs, withURLParam(req, "id", testWorkspaceID))
}

func TestListWorkspaceLocalDirs(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	// One shared tree: alpha/nested (a directory one level down), beta
	// (empty), a hidden directory, and a plain file that must never show up.
	root := t.TempDir()
	for _, dir := range []string{"alpha", filepath.Join("alpha", "nested"), "beta", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	noteFile := filepath.Join(root, "note.txt")
	if err := os.WriteFile(noteFile, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("write note: %v", err)
	}

	dirNames := func(resp localDirsResponse) []string {
		names := make([]string, 0, len(resp.Dirs))
		for _, d := range resp.Dirs {
			names = append(names, d.Name)
		}
		return names
	}
	eq := func(got, want []string) bool {
		return len(got) == len(want) && strings.Join(got, ",") == strings.Join(want, ",")
	}

	cases := []struct {
		name  string
		query func(t *testing.T) string
		want  int
		check func(t *testing.T, resp *testutil.Response)
	}{
		{
			// The blacklist gate refuses the whole request, not just the
			// children — and with the blacklist function's reason verbatim.
			name: "blacklisted system root is rejected with its reason",
			query: func(*testing.T) string {
				return "path=" + url.QueryEscape(blacklistedSystemRoot())
			},
			want: 400,
			check: func(t *testing.T, resp *testutil.Response) {
				if msg, _ := resp.Map()["error"].(string); !strings.Contains(msg, "protected system root") {
					t.Fatalf("error = %q, want it to contain %q", msg, "protected system root")
				}
			},
		},
		{
			name: "relative path is rejected",
			query: func(*testing.T) string {
				return "path=" + url.QueryEscape("relative/project")
			},
			want: 400,
			check: func(t *testing.T, resp *testutil.Response) {
				if msg, _ := resp.Map()["error"].(string); !strings.Contains(msg, "absolute") {
					t.Fatalf("error = %q, want it to mention an absolute path", msg)
				}
			},
		},
		{
			name: "missing directory is 404",
			query: func(t *testing.T) string {
				return "path=" + url.QueryEscape(filepath.Join(root, "does-not-exist"))
			},
			want: 404,
		},
		{
			name: "regular file is 400",
			query: func(*testing.T) string {
				return "path=" + url.QueryEscape(filepath.Join(root, "note.txt"))
			},
			want: 400,
		},
		{
			name: "lists subdirectories only, hidden skipped, files never",
			query: func(*testing.T) string {
				return "path=" + url.QueryEscape(root)
			},
			want: 200,
			check: func(t *testing.T, resp *testutil.Response) {
				var out localDirsResponse
				resp.JSON(&out)
				if !eq(dirNames(out), []string{"alpha", "beta"}) {
					t.Fatalf("dirs = %v, want exactly [alpha beta]", dirNames(out))
				}
				if out.Dirs[0].HasChildren != true || out.Dirs[1].HasChildren != false {
					t.Fatalf("has_children = [%v %v], want [true false]",
						out.Dirs[0].HasChildren, out.Dirs[1].HasChildren)
				}
				for _, d := range out.Dirs {
					if d.Blocked {
						t.Fatalf("dir %q blocked, want selectable", d.Name)
					}
					if want := filepath.Join(root, d.Name); d.Path != want {
						t.Fatalf("dir %q path = %q, want %q", d.Name, d.Path, want)
					}
				}
				if out.Path != filepath.Clean(root) {
					t.Fatalf("path = %q, want cleaned %q", out.Path, filepath.Clean(root))
				}
				// The workspace's only seeded runtime carries a fixed test
				// device name and a NULL daemon_id: no online daemon matches
				// this machine, and the client must see that explicitly.
				if out.DaemonStatus != "none" || out.DaemonID != "" {
					t.Fatalf("daemon = %q/%q, want none/\"\"", out.DaemonStatus, out.DaemonID)
				}
			},
		},
		{
			name: "include_hidden=1 surfaces dot directories",
			query: func(*testing.T) string {
				return "path=" + url.QueryEscape(root) + "&include_hidden=1"
			},
			want: 200,
			check: func(t *testing.T, resp *testutil.Response) {
				var out localDirsResponse
				resp.JSON(&out)
				if !eq(dirNames(out), []string{".hidden", "alpha", "beta"}) {
					t.Fatalf("dirs = %v, want [.hidden alpha beta]", dirNames(out))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := getLocalDirs(t, tc.query(t))
			resp.Want(tc.want)
			if tc.check != nil {
				tc.check(t, resp)
			}
		})
	}

	t.Run("no path defaults to the server home directory", func(t *testing.T) {
		// os.UserHomeDir on unix is $HOME verbatim, so pointing HOME at the
		// fixture tree also exercises the deliberate home exemption: the
		// daemon refuses $HOME as a project ROOT, but browsing its listing is
		// the endpoint's documented default view.
		t.Setenv("HOME", filepath.Clean(root))
		resp := getLocalDirs(t, "")
		resp.Want(200)
		var out localDirsResponse
		resp.JSON(&out)
		if out.Path != filepath.Clean(root) {
			t.Fatalf("path = %q, want default home %q", out.Path, filepath.Clean(root))
		}
		if out.Home != filepath.Clean(root) {
			t.Fatalf("home = %q, want %q", out.Home, filepath.Clean(root))
		}
		if !eq(dirNames(out), []string{"alpha", "beta"}) {
			t.Fatalf("dirs = %v, want [alpha beta]", dirNames(out))
		}
	})

	t.Run("an online daemon on this hostname resolves", func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			t.Skip("hostname unavailable")
		}
		// device_info as the registration handler stores it: the device name
		// with " · <version>" appended when the runtime reports one.
		dbfx.Runtime(t, "browse-resolved", testutil.Cols{
			"daemon_id":   "daemon-browse-resolved",
			"device_info": hostname + " · codex-cli 0.1.0",
			"provider":    "browse_test_a",
			"status":      "online",
		})
		resp := getLocalDirs(t, "path="+url.QueryEscape(root))
		resp.Want(200)
		var out localDirsResponse
		resp.JSON(&out)
		if out.DaemonStatus != "resolved" || out.DaemonID != "daemon-browse-resolved" {
			t.Fatalf("daemon = %q/%q, want resolved/daemon-browse-resolved",
				out.DaemonStatus, out.DaemonID)
		}
	})

	t.Run("two online daemons on this hostname are ambiguous", func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			t.Skip("hostname unavailable")
		}
		dbfx.Runtime(t, "browse-amb-1", testutil.Cols{
			"daemon_id":   "daemon-browse-amb-1",
			"device_info": hostname,
			"provider":    "browse_test_b1",
			"status":      "online",
		})
		dbfx.Runtime(t, "browse-amb-2", testutil.Cols{
			"daemon_id":   "daemon-browse-amb-2",
			"device_info": hostname,
			"provider":    "browse_test_b2",
			"status":      "online",
		})
		resp := getLocalDirs(t, "path="+url.QueryEscape(root))
		resp.Want(200)
		var out localDirsResponse
		resp.JSON(&out)
		if out.DaemonStatus != "ambiguous" || out.DaemonID != "" {
			t.Fatalf("daemon = %q/%q, want ambiguous/empty", out.DaemonStatus, out.DaemonID)
		}
	})
}
