package localpath

import (
	"runtime"
	"testing"
)

// TestIsDriveRoot covers the Windows drive-root generalisation. Static
// enumeration in the old blacklist (C..F) missed mounts at G:\ and up; the
// new check goes through filepath.VolumeName so any drive letter (and UNC
// roots) is rejected.
func TestIsDriveRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		// filepath.VolumeName returns "" on POSIX, so IsDriveRoot always
		// returns false off Windows. The semantic contract is enforced by
		// the early `runtime.GOOS != "windows"` guard; the case table
		// below is only meaningful on a Windows runner.
		t.Skip("windows-only behaviour")
	}
	cases := []struct {
		p    string
		want bool
	}{
		{`C:\`, true},
		{`G:\`, true},
		{`Z:\`, true},
		{`C:/`, true},
		{`C:`, true},
		{`\\srv\share`, true},
		{`\\srv\share\`, true},
		{`C:\Users`, false},
		{`D:\proj`, false},
		{`C:\Users\me\code`, false},
	}
	for _, c := range cases {
		if got := IsDriveRoot(c.p); got != c.want {
			t.Errorf("IsDriveRoot(%q) = %v, want %v", c.p, got, c.want)
		}
	}
}
