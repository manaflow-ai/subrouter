//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPrivateProxyParentAllowACETypeClassification(t *testing.T) {
	for _, test := range []struct {
		name        string
		aceType     uint8
		standard    bool
		unsupported bool
	}{
		{name: "standard allow", aceType: windows.ACCESS_ALLOWED_ACE_TYPE, standard: true},
		{name: "compound allow", aceType: 0x4, unsupported: true},
		{name: "object allow", aceType: 0x5, unsupported: true},
		{name: "callback allow", aceType: 0x9, unsupported: true},
		{name: "callback object allow", aceType: 0xb, unsupported: true},
		{name: "deny", aceType: windows.ACCESS_DENIED_ACE_TYPE},
	} {
		t.Run(test.name, func(t *testing.T) {
			standard, unsupported := privateProxyParentAllowACEType(test.aceType)
			if standard != test.standard || unsupported != test.unsupported {
				t.Fatalf("classification = (%t, %t), want (%t, %t)", standard, unsupported, test.standard, test.unsupported)
			}
		})
	}
}

func TestRemovePrivateProxyHomeClearsReadonlyRegularFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	readonly := filepath.Join(home, "readonly")
	if err := os.WriteFile(readonly, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	pathUTF16, err := windows.UTF16PtrFromString(readonly)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(pathUTF16, windows.FILE_ATTRIBUTE_READONLY); err != nil {
		t.Fatal(err)
	}

	if err := removePrivateProxyHome(home); err != nil {
		t.Fatalf("remove private home containing read-only file: %v", err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private home survived cleanup: %v", err)
	}
}

func TestRemovePrivateProxyHomeAllowsCurrentUserTempParent(t *testing.T) {
	home, err := os.MkdirTemp("", "subrouter-private-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.WriteFile(filepath.Join(home, "temporary"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removePrivateProxyHome(home); err != nil {
		t.Fatalf("remove private home under the normal temporary directory: %v", err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private home survived cleanup: %v", err)
	}
}

func TestRemovePrivateProxyHomeRejectsUntrustedWritableParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared-parent")
	home := filepath.Join(parent, "private-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
			},
		},
		{
			AccessPermissions: windows.FILE_GENERIC_WRITE,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(everyone),
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		parent,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		t.Fatal(err)
	}

	err = removePrivateProxyHome(home)
	if err == nil || !strings.Contains(err.Error(), "grants write access") {
		t.Fatalf("untrusted writable parent was accepted: %v", err)
	}
	if _, err := os.Lstat(home); err != nil {
		t.Fatalf("home under rejected parent was changed: %v", err)
	}
}

func TestRemovePrivateProxyHomeRejectsReparsePointParent(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	home := filepath.Join(external, "private-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "parent-link")
	createWindowsTestJunction(t, parent, external)

	err := removePrivateProxyHome(filepath.Join(parent, "private-home"))
	if err == nil || !strings.Contains(err.Error(), "traverses reparse point") {
		t.Fatalf("reparse-point parent was accepted: %v", err)
	}
	if _, err := os.Lstat(home); err != nil {
		t.Fatalf("home behind rejected parent was changed: %v", err)
	}
}

func TestRemovePrivateProxyHomeRejectsIntermediateReparsePoint(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	parent := filepath.Join(external, "parent")
	home := filepath.Join(parent, "private-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(root, "intermediate-link")
	createWindowsTestJunction(t, intermediate, external)

	err := removePrivateProxyHome(filepath.Join(intermediate, "parent", "private-home"))
	if err == nil || !strings.Contains(err.Error(), "traverses reparse point") {
		t.Fatalf("intermediate reparse point was accepted: %v", err)
	}
	if _, err := os.Lstat(home); err != nil {
		t.Fatalf("home behind rejected intermediate was changed: %v", err)
	}
}

func TestPreparedPrivateProxyCleanupUsesPinnedHomeAfterReplacement(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "private-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	originalMarker := filepath.Join(home, "remove-from-pinned-home")
	if err := os.WriteFile(originalMarker, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup, err := preparePrivateProxyHomeCleanup(home)
	if err != nil {
		t.Fatal(err)
	}
	detached := home + "-detached"
	if err := os.Rename(home, detached); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(detached) })
	external := t.TempDir()
	externalMarker := filepath.Join(external, "must-survive")
	if err := os.WriteFile(externalMarker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	createWindowsTestJunction(t, home, external)

	err = cleanup()
	if err == nil || !strings.Contains(err.Error(), "basename was replaced") {
		t.Fatalf("replacement race was not reported: %v", err)
	}
	if _, err := os.Lstat(originalMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned original contents survived cleanup: %v", err)
	}
	if got, err := os.ReadFile(externalMarker); err != nil || string(got) != "safe" {
		t.Fatalf("cleanup followed replacement junction: %q, %v", got, err)
	}
	if info, err := os.Lstat(home); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement junction was changed: info=%v err=%v", info, err)
	}
}

func TestRemovePrivateProxyHomeDoesNotFollowNestedJunction(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-home")
	if err := os.MkdirAll(filepath.Join(home, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	marker := filepath.Join(external, "must-survive")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	createWindowsTestJunction(t, filepath.Join(home, "child", "outside"), external)

	if err := removePrivateProxyHome(home); err != nil {
		t.Fatalf("remove private home with nested junction: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "safe" {
		t.Fatalf("cleanup followed nested junction: %q, %v", got, err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private home survived cleanup: %v", err)
	}
}

func TestRemovePrivateProxyHomeDoesNotFollowReplacedRootJunction(t *testing.T) {
	home := filepath.Join(t.TempDir(), "private-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	detached := home + "-detached"
	if err := os.Rename(home, detached); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(detached) })
	external := t.TempDir()
	marker := filepath.Join(external, "must-survive")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	createWindowsTestJunction(t, home, external)

	if err := removePrivateProxyHome(home); err != nil {
		t.Fatalf("remove replaced root junction: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "safe" {
		t.Fatalf("cleanup followed replaced root junction: %q, %v", got, err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement junction survived cleanup: %v", err)
	}
	if _, err := os.Lstat(detached); err != nil {
		t.Fatalf("detached original home was removed: %v", err)
	}
}

func createWindowsTestJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("create test junction: %v: %s", err, output)
	}
}
