// Package localpath is the single source of truth for the filesystem
// blacklist that guards local_directory project resources. The daemon
// refuses to run an agent inside a path that maps to the whole machine or an
// entire user profile; the server-side directory browser applies the exact
// same rules so a Web client can never list or pick a directory the daemon
// would later reject. Both sides must agree — a divergence here would show
// the user a picker that offers paths their own machine will refuse.
//
// The functions here return a human-readable reason and never prepend the
// "local_directory: " error prefix: callers own the wording of their failure
// channel (the daemon formats task-fail comments, the handler writes HTTP
// error bodies).
package localpath

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// IsBlacklisted rejects paths that map to the whole machine or an entire user
// profile. The intent is to keep the daemon from accidentally stamping
// context files (.agent_context/, .claude/skills/, .multica/) at the root of
// a user's account or the OS — a misconfiguration on the UI side should fail
// fast rather than litter the user's home.
//
// The check is by literal equality after Clean(), not prefix containment: a
// legitimate project under /Users/<user>/code/proj should pass.
func IsBlacklisted(absPath string) (reason string, blocked bool) {
	cleaned := filepath.Clean(absPath)
	if IsDriveRoot(cleaned) {
		return fmt.Sprintf("path is a drive root %q", cleaned), true
	}
	for _, banned := range SystemRootBlacklist() {
		if cleaned == banned {
			return fmt.Sprintf("path is a protected system root %q", banned), true
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if cleaned == filepath.Clean(home) {
			return "path is the user's home directory", true
		}
	}
	return "", false
}

// IsBlacklistedRealPath is the canonical-aware variant of IsBlacklisted. It
// compares the symlink-resolved realPath against the symlink-resolved form of
// each blacklist entry so OS-level redirects (notably macOS's /etc ->
// /private/etc, /tmp -> /private/tmp, /var -> /private/var) cannot be used to
// slip a candidate past the literal blacklist — whether the redirect is
// reached via a user-created symlink (~/proj/home-link -> /Users/me) or by
// directly typing the canonical form (/private/tmp), which is identical to
// the OS view of /tmp.
func IsBlacklistedRealPath(realPath string) (reason string, blocked bool) {
	realClean := filepath.Clean(realPath)
	if IsDriveRoot(realClean) {
		return fmt.Sprintf("path is a drive root %q", realClean), true
	}
	for _, banned := range SystemRootBlacklist() {
		bannedClean := filepath.Clean(banned)
		if realClean == bannedClean {
			return fmt.Sprintf("path is a protected system root %q", banned), true
		}
		if r, err := filepath.EvalSymlinks(banned); err == nil {
			if filepath.Clean(r) == realClean {
				return fmt.Sprintf("path is a protected system root %q", banned), true
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		homeClean := filepath.Clean(home)
		if realClean == homeClean {
			return "path is the user's home directory", true
		}
		if r, err := filepath.EvalSymlinks(home); err == nil {
			if filepath.Clean(r) == realClean {
				return "path is the user's home directory", true
			}
		}
	}
	return "", false
}

// IsDriveRoot reports whether absPath is the root of a Windows volume — any
// of `C:\`, `D:\`, ..., `Z:\`, plus less common cases like `\\server\share`
// (filepath.VolumeName treats UNC roots as volumes too). On non-Windows
// this is always false because POSIX has no concept of drive letters and
// `/` is covered by SystemRootBlacklist.
//
// We rely on filepath.VolumeName rather than enumerating drive letters
// statically: removable / network drives can be mounted at any letter
// (`G:\`, `H:\`, ...), and Windows installs are increasingly happy to put
// the user profile on a non-C drive. A static list (C..F) would miss them
// all.
func IsDriveRoot(absPath string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	vol := filepath.VolumeName(absPath)
	if vol == "" {
		return false
	}
	// VolumeName returns the volume without trailing separator (`C:` or
	// `\\srv\share`). A drive root is volume + one separator (or, after
	// filepath.Clean, just the volume on bare-volume input).
	rest := absPath[len(vol):]
	return rest == "" || rest == `\` || rest == "/"
}

// SystemRootBlacklist returns the per-OS list of paths the daemon never
// allows as a local_directory root. POSIX systems get `/`, `/Users`, `/home`
// (and macOS's `/Users/Shared` for good measure); Windows gets the
// well-known account / shared trees under C:. Drive roots themselves are
// handled by IsDriveRoot so we don't have to enumerate G:\, H:\, etc.
// The list is intentionally conservative — it errs on the side of
// rejecting more, since the desktop UI is expected to surface a friendly
// picker that never produces these values.
func SystemRootBlacklist() []string {
	if runtime.GOOS == "windows" {
		return []string{`C:\Users`, `C:\ProgramData`, `C:\Program Files`, `C:\Program Files (x86)`, `C:\Windows`}
	}
	return []string{"/", "/Users", "/Users/Shared", "/home", "/root", "/var", "/etc", "/tmp", "/usr", "/opt"}
}
