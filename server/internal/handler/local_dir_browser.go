package handler

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/localpath"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Daemon-resolution verdicts for LocalDirListingResponse.DaemonStatus.
// "resolved" carries exactly one online daemon for this machine, "none" and
// "ambiguous" leave DaemonID empty — the client then falls back to manual
// path entry or the desktop picker instead of guessing which machine owns a
// local_directory resource.
const (
	localDirDaemonResolved  = "resolved"
	localDirDaemonNone      = "none"
	localDirDaemonAmbiguous = "ambiguous"
)

// LocalDirBrowserEntry is one browsable subdirectory of the requested path.
// Blocked marks a directory the daemon would refuse as a local_directory
// project root (system roots, the user's own home); the browser still lists
// it so the UI can grey it out instead of silently dropping entries.
type LocalDirBrowserEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	HasChildren bool   `json:"has_children"`
	Blocked     bool   `json:"blocked"`
}

// LocalDirListingResponse is the GET /api/workspaces/{id}/local-dirs shape.
// Parent is "" at the filesystem root or whenever the parent is itself
// browser-blocked, so the client can disable "up one level". Hostname and
// Home describe the machine whose filesystem is being browsed; DaemonID /
// DaemonStatus bind it to the workspace's registered daemon (see
// localDirDaemon* above).
type LocalDirListingResponse struct {
	Path         string                 `json:"path"`
	Parent       string                 `json:"parent"`
	Hostname     string                 `json:"hostname"`
	Home         string                 `json:"home"`
	DaemonID     string                 `json:"daemon_id"`
	DaemonStatus string                 `json:"daemon_status"`
	Dirs         []LocalDirBrowserEntry `json:"dirs"`
}

// ListWorkspaceLocalDirs browses the API machine's directory tree so a web
// client can pick a local_directory project resource without the desktop
// app. Read-only, admin-gated (see router.go): it exposes the deployment
// host's filesystem to any workspace owner/admin, a far wider surface than
// the desktop flow where the user picks a folder on their own machine.
//
// Path safety is shared with the daemon through internal/localpath — the
// same blacklist that refuses a local_directory rooted at a system root or
// a user's home decides what may not be listed or descended into, and the
// symlink-resolved re-check there blocks the ~/link-to-etc / /private/tmp
// bypasses exactly like validateLocalPath does. The one deliberate
// difference is documented on browseBlockedReason: listing the user's home
// read-only is allowed (and is the default view) even though the daemon
// would refuse to run an agent rooted there.
func (h *Handler) ListWorkspaceLocalDirs(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	home, homeErr := os.UserHomeDir()

	query := r.URL.Query()
	target := strings.TrimSpace(query.Get("path"))
	if target == "" {
		// No explicit path opens on the server user's home. Falling back to
		// "/" instead would hit the system-root blacklist anyway — and
		// silently listing the root of the machine is the last thing this
		// endpoint should do — so an unresolvable home is an explicit error.
		if homeErr != nil || home == "" {
			writeError(w, http.StatusBadRequest, "server could not determine a user home directory to browse; pass an absolute path")
			return
		}
		target = home
	}
	if !filepath.IsAbs(target) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("path must be absolute, got %q", target))
		return
	}
	target = filepath.Clean(target)

	if reason := browseBlockedReason(target, false); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}

	info, err := os.Stat(target)
	switch {
	case err != nil && os.IsNotExist(err):
		writeError(w, http.StatusNotFound, fmt.Sprintf("directory does not exist: %q", target))
		return
	case err != nil && os.IsPermission(err):
		writeError(w, http.StatusForbidden, fmt.Sprintf("permission denied for %q", target))
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("stat %q: %v", target, err))
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("path is not a directory: %q", target))
		return
	}

	// Double check after resolving symlinks — the same two-step the
	// daemon's validateLocalPath runs, so a link (or a symlinked parent
	// component) cannot route the view into a blacklisted target the
	// literal check waved through.
	realPath, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("directory does not exist: %q", target))
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("resolve symlinks for %q: %v", target, err))
		return
	}
	if reason := browseBlockedReason(realPath, true); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		if os.IsPermission(err) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("cannot read %q: permission denied", target))
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot read %q: %v", target, err))
		return
	}

	includeHidden := query.Get("include_hidden") == "1"
	dirs := make([]LocalDirBrowserEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			continue
		}
		// Directories only, and os.ReadDir does not follow links: an entry
		// whose type is ModeSymlink reports IsDir() == false even when it
		// points at a directory. The browser deliberately skips those — a
		// link is only safely navigable after the resolve-and-recheck dance
		// this handler runs per explicit request, not once per listed child.
		if !entry.IsDir() {
			continue
		}
		childPath := filepath.Join(target, name)
		blocked := browseBlockedReason(childPath, false) != ""
		hasChildren, readable := dirHasSubdirs(childPath)
		if !readable && !blocked {
			// Unreadable children are dropped: the daemon could never use
			// them and a click would only 403. Blacklisted ones stay in the
			// list flagged so the UI can explain why they are greyed out.
			continue
		}
		dirs = append(dirs, LocalDirBrowserEntry{
			Name:        name,
			Path:        childPath,
			HasChildren: hasChildren,
			Blocked:     blocked,
		})
	}

	// Parent navigation. Reaching the filesystem root is Dir(x) == x; a
	// browser-blocked parent (a system root one level up) collapses to ""
	// so the client disables "up one level" instead of offering a 400.
	parent := ""
	if p := filepath.Dir(target); p != target {
		if browseBlockedReason(p, false) == "" {
			parent = p
		}
	}

	hostname, _ := os.Hostname() // "" on failure: clients render daemon_status "none"
	daemonID, daemonStatus := "", localDirDaemonNone
	if hostname != "" {
		ids, err := h.Queries.ListOnlineDaemonIDsByDevice(r.Context(), db.ListOnlineDaemonIDsByDeviceParams{
			WorkspaceID: wsUUID,
			DeviceInfo:  hostname,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to resolve the daemon on this machine")
			return
		}
		switch len(ids) {
		case 0:
		case 1:
			// daemon_id IS NOT NULL is in the query, so Valid holds; guard
			// anyway rather than resolve a NULL to the empty-string daemon.
			if ids[0].Valid {
				daemonID, daemonStatus = ids[0].String, localDirDaemonResolved
			}
		default:
			daemonStatus = localDirDaemonAmbiguous
		}
	}

	writeJSON(w, http.StatusOK, LocalDirListingResponse{
		Path:         target,
		Parent:       parent,
		Hostname:     hostname,
		Home:         home,
		DaemonID:     daemonID,
		DaemonStatus: daemonStatus,
		Dirs:         dirs,
	})
}

// browseBlockedReason returns the blacklist reason that forbids LISTING
// cleaned (a filepath.Clean'd absolute path, or its symlink-resolved form
// when isRealPath is true), or "" when the directory may be browsed.
//
// The one deliberate divergence from the daemon gate: the "user's home
// directory" entry does not block browsing. That entry exists so a
// local_directory resource never roots an AGENT at $HOME — the daemon
// stamps context files and runs edits there. This endpoint only reads a
// listing, home is the natural landing view (the default when no path is
// given), and the response contract reports home and exposes it as a
// navigable parent. System roots and drive roots still refuse: one level
// below any of them the damage is not "a listing was seen" but whole-disk
// enumeration, and the daemon could never use one anyway. The blocked
// flag on child entries keeps using the FULL blacklist via
// localpath.IsBlacklisted, so home still renders as unselectable.
func browseBlockedReason(cleaned string, isRealPath bool) string {
	var reason string
	var blocked bool
	if isRealPath {
		reason, blocked = localpath.IsBlacklistedRealPath(cleaned)
	} else {
		reason, blocked = localpath.IsBlacklisted(cleaned)
	}
	if !blocked {
		return ""
	}
	if isUserHome(cleaned) {
		return ""
	}
	return reason
}

// isUserHome reports whether cleaned equals the server user's home, either
// literally or through its symlink-resolved form — the exact pair of
// comparisons localpath runs for its home entry.
func isUserHome(cleaned string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if filepath.Clean(home) == cleaned {
		return true
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		if filepath.Clean(real) == cleaned {
			return true
		}
	}
	return false
}

// dirHasSubdirs peeks one level into dir — enough for the tree's expand
// affordance without recursing per child — and reports whether the peek
// worked. readable == false covers missing and permission-denied children;
// the caller drops those (unless blacklisted, where the flag alone matters).
func dirHasSubdirs(dir string) (hasChildren, readable bool) {
	f, err := os.Open(dir)
	if err != nil {
		return false, false
	}
	defer f.Close()
	for {
		batch, err := f.ReadDir(64)
		for _, e := range batch {
			if e.IsDir() {
				return true, true
			}
		}
		if err != nil {
			// io.EOF (or a mid-scan read error) — the directory was openable,
			// so it is readable; it just holds no subdirectory.
			return false, true
		}
	}
}
