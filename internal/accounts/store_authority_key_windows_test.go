//go:build windows

package accounts

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStoreAuthorityAllowACETypeClassification(t *testing.T) {
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
			standard, unsupported := storeAuthorityAllowACEType(test.aceType)
			if standard != test.standard || unsupported != test.unsupported {
				t.Fatalf("classification = (%t, %t), want (%t, %t)", standard, unsupported, test.standard, test.unsupported)
			}
		})
	}
}

func TestOpenPrivateStoreAuthorityKeyAcceptsProtectedCurrentUserOnlyAccess(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0x5a}, 32)
	path := filepath.Join(parent, "authority-key")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{parent, path} {
		if err := windows.SetNamedSecurityInfo(
			target,
			windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil,
			nil,
			acl,
			nil,
		); err != nil {
			t.Fatal(err)
		}
	}

	file, err := openPrivateStoreAuthorityKey(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("authority key = %x, want %x", got, want)
	}
}

func TestOpenPrivateStoreAuthorityKeyRejectsUntrustedAllowedAccess(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		mask windows.ACCESS_MASK
	}{
		{name: "read", mask: windows.GENERIC_READ},
		{name: "non-read nonzero", mask: windows.SYNCHRONIZE},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "authority-key")
			if err := os.WriteFile(path, make([]byte, 32), 0o600); err != nil {
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
					AccessPermissions: test.mask,
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
				path,
				windows.SE_FILE_OBJECT,
				windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
				nil,
				nil,
				acl,
				nil,
			); err != nil {
				t.Fatal(err)
			}

			file, err := openPrivateStoreAuthorityKey(path)
			if file != nil {
				_ = file.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "grants access outside") {
				t.Fatalf("untrusted allow ACE was accepted: %v", err)
			}
		})
	}
}

func TestOpenPrivateStoreAuthorityKeyRejectsUntrustedWritableParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "authority-key")
	if err := os.WriteFile(path, make([]byte, 32), 0o600); err != nil {
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

	file, err := openPrivateStoreAuthorityKey(path)
	if file != nil {
		_ = file.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "parent grants write access") {
		t.Fatalf("untrusted writable parent was accepted: %v", err)
	}
}

func TestOpenPrivateStoreAuthorityKeyRejectsReparsePointParent(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	path := filepath.Join(external, "authority-key")
	if err := os.WriteFile(path, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "parent-link")
	createStoreAuthorityTestJunction(t, parent, external)

	file, err := openPrivateStoreAuthorityKey(filepath.Join(parent, "authority-key"))
	if file != nil {
		_ = file.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "traverses reparse point") {
		t.Fatalf("reparse-point parent was accepted: %v", err)
	}
}

func TestOpenPrivateStoreAuthorityKeyRejectsIntermediateReparsePoint(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	parent := filepath.Join(external, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "authority-key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	intermediate := filepath.Join(root, "intermediate-link")
	createStoreAuthorityTestJunction(t, intermediate, external)

	file, err := openPrivateStoreAuthorityKey(filepath.Join(intermediate, "parent", "authority-key"))
	if file != nil {
		_ = file.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "traverses reparse point") {
		t.Fatalf("intermediate reparse point was accepted: %v", err)
	}
}

func createStoreAuthorityTestJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("create test junction: %v: %s", err, output)
	}
}
