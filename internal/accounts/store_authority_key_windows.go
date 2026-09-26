//go:build windows

package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openPrivateStoreAuthorityKey(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parentPath, name := filepath.Dir(absolute), filepath.Base(absolute)
	if err := rejectStoreAuthorityParentReparsePoints(parentPath); err != nil {
		return nil, err
	}
	parentHandle, err := openPinnedStoreAuthorityParent(parentPath)
	if err != nil {
		return nil, err
	}
	closeParent := func() { _ = windows.CloseHandle(parentHandle) }
	if err := validateStoreAuthorityParentTrust(parentHandle); err != nil {
		closeParent()
		return nil, err
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		closeParent()
		return nil, err
	}
	if err := verifyStoreAuthorityRootIdentity(root, parentHandle); err != nil {
		_ = root.Close()
		closeParent()
		return nil, err
	}
	entry, err := root.Lstat(name)
	if err != nil || !entry.Mode().IsRegular() {
		_ = root.Close()
		closeParent()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("account store authority key must be a private regular file")
	}
	handle, err := openStoreAuthorityKeyRelative(parentHandle, name)
	rootCloseErr := root.Close()
	closeParent()
	if err != nil {
		return nil, err
	}
	if rootCloseErr != nil {
		_ = windows.CloseHandle(handle)
		return nil, rootCloseErr
	}
	file := os.NewFile(uintptr(handle), absolute)
	closeOnError := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		return closeOnError(err)
	}
	if opened.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return closeOnError(fmt.Errorf("account store authority key must be a private regular file"))
	}
	if err := validateStoreAuthorityKeyTrust(handle); err != nil {
		return closeOnError(err)
	}
	return file, nil
}

func rejectStoreAuthorityParentReparsePoints(path string) error {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return fmt.Errorf("account store authority parent has no volume")
	}
	current := volume + string(filepath.Separator)
	relative := strings.TrimLeft(path[len(volume):], `\/`)
	for _, component := range strings.FieldsFunc(relative, func(r rune) bool {
		return r == '\\' || r == '/'
	}) {
		current = filepath.Join(current, component)
		pathUTF16, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(pathUTF16)
		if err != nil {
			return err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("account store authority parent traverses reparse point %q", current)
		}
		if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return fmt.Errorf("account store authority parent component is not a directory: %q", current)
		}
	}
	return nil
}

func openPinnedStoreAuthorityParent(path string) (windows.Handle, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.READ_CONTROL|windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, err
	}
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	if opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("account store authority parent must be a regular directory")
	}
	return handle, nil
}

func validateStoreAuthorityParentTrust(handle windows.Handle) error {
	user, trusted, err := storeAuthorityTrustedSIDs()
	if err != nil {
		return err
	}
	const fileDeleteChild = windows.ACCESS_MASK(0x00000040)
	writeMask := windows.ACCESS_MASK(
		windows.GENERIC_ALL|windows.GENERIC_WRITE|windows.FILE_WRITE_DATA|
			windows.FILE_APPEND_DATA|windows.FILE_WRITE_EA|windows.FILE_WRITE_ATTRIBUTES|
			windows.WRITE_DAC|windows.WRITE_OWNER|windows.DELETE,
	) | fileDeleteChild
	return validateStoreAuthorityACL(handle, user, trusted, writeMask, "parent")
}

func validateStoreAuthorityKeyTrust(handle windows.Handle) error {
	user, trusted, err := storeAuthorityTrustedSIDs()
	if err != nil {
		return err
	}
	return validateStoreAuthorityACL(handle, user, trusted, ^windows.ACCESS_MASK(0), "key")
}

func storeAuthorityTrustedSIDs() (*windows.SID, []*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	trusted := []*windows.SID{user.User.Sid}
	for _, sidType := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinBuiltinAdministratorsSid,
		windows.WinLocalSystemSid,
	} {
		sid, err := windows.CreateWellKnownSid(sidType)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect account store authority ACL: %w", err)
		}
		trusted = append(trusted, sid)
	}
	return user.User.Sid, trusted, nil
}

func validateStoreAuthorityACL(handle windows.Handle, user *windows.SID, trusted []*windows.SID, rejectMask windows.ACCESS_MASK, kind string) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("inspect account store authority %s ACL: %w", kind, err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("inspect account store authority %s owner: %w", kind, err)
	}
	if owner == nil || !owner.Equals(user) {
		return fmt.Errorf("account store authority %s must be owned by the current user", kind)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("account store authority %s must have a protected access list", kind)
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("inspect account store authority %s ACL entry: %w", kind, err)
		}
		if ace == nil || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask&rejectMask == 0 {
			continue
		}
		standard, unsupported := storeAuthorityAllowACEType(ace.Header.AceType)
		if unsupported {
			return fmt.Errorf("account store authority %s has unsupported allow ACE type %d", kind, ace.Header.AceType)
		}
		if !standard {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trustedSID := false
		for _, allowed := range trusted {
			if aceSID.Equals(allowed) {
				trustedSID = true
				break
			}
		}
		if !trustedSID {
			if kind == "parent" {
				return fmt.Errorf("account store authority parent grants write access to an untrusted principal")
			}
			return fmt.Errorf("account store authority key grants access outside trusted principals")
		}
	}
	return nil
}

func storeAuthorityAllowACEType(aceType uint8) (standard, unsupported bool) {
	const (
		accessAllowedCompoundACEType       = 0x4
		accessAllowedObjectACEType         = 0x5
		accessAllowedCallbackACEType       = 0x9
		accessAllowedCallbackObjectACEType = 0xb
	)
	switch aceType {
	case windows.ACCESS_ALLOWED_ACE_TYPE:
		return true, false
	case accessAllowedCompoundACEType, accessAllowedObjectACEType,
		accessAllowedCallbackACEType, accessAllowedCallbackObjectACEType:
		return false, true
	default:
		return false, false
	}
}

func verifyStoreAuthorityRootIdentity(root *os.Root, pinned windows.Handle) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	var want, got windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(pinned, &want); err != nil {
		return err
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(directory.Fd()), &got); err != nil {
		return err
	}
	if want.VolumeSerialNumber != got.VolumeSerialNumber ||
		want.FileIndexHigh != got.FileIndexHigh || want.FileIndexLow != got.FileIndexLow {
		return fmt.Errorf("account store authority parent changed while opening rooted access")
	}
	return nil
}

func openStoreAuthorityKeyRelative(parent windows.Handle, name string) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return 0, err
	}
	return handle, nil
}
