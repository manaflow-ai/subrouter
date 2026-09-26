//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// removePrivateProxyHome removes a child-controlled directory tree using
// handle-relative operations, so a junction inserted during cleanup cannot
// redirect traversal outside the temporary home.
func removePrivateProxyHome(path string) error {
	path = filepath.Clean(path)
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	if name == "." || parentPath == path {
		return fmt.Errorf("refuse to remove private proxy home %q", path)
	}
	parent, err := openPrivateProxyParent(parentPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open private proxy home parent: %w", err)
	}
	removeErr := removePrivateProxyEntryInRoot(parent, name)
	closeErr := parent.Close()
	return errors.Join(removeErr, closeErr)
}

func preparePrivateProxyHomeCleanup(path string) (func() error, error) {
	path = filepath.Clean(path)
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	if name == "." || parentPath == path {
		return nil, fmt.Errorf("refuse to prepare private proxy home cleanup for %q", path)
	}
	parent, err := openPrivateProxyParent(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open private proxy home parent: %w", err)
	}
	info, err := parent.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_ = parent.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect private proxy home: %w", err)
		}
		return nil, fmt.Errorf("private proxy home must be a regular directory")
	}
	home, err := parent.OpenRoot(name)
	if err != nil {
		_ = parent.Close()
		return nil, fmt.Errorf("pin private proxy home: %w", err)
	}
	homeIdentity, err := privateProxyRootIdentity(home)
	if err != nil {
		_ = home.Close()
		_ = parent.Close()
		return nil, fmt.Errorf("identify pinned private proxy home: %w", err)
	}
	var once sync.Once
	var cleanupErr error
	cleanup := func() error {
		once.Do(func() {
			contentsErr := removePrivateProxyRootContents(home)
			homeCloseErr := home.Close()
			var removeErr error
			if contentsErr == nil {
				removeErr = removePinnedPrivateProxyHomeBasename(parent, name, homeIdentity)
			}
			parentCloseErr := parent.Close()
			cleanupErr = errors.Join(contentsErr, homeCloseErr, removeErr, parentCloseErr)
		})
		return cleanupErr
	}
	return cleanup, nil
}

type privateProxyFileIdentity struct {
	volumeSerialNumber uint32
	fileIndexHigh      uint32
	fileIndexLow       uint32
}

func privateProxyRootIdentity(root *os.Root) (privateProxyFileIdentity, error) {
	directory, err := root.Open(".")
	if err != nil {
		return privateProxyFileIdentity{}, err
	}
	defer directory.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(directory.Fd()), &info); err != nil {
		return privateProxyFileIdentity{}, err
	}
	return privateProxyFileIdentity{
		volumeSerialNumber: info.VolumeSerialNumber,
		fileIndexHigh:      info.FileIndexHigh,
		fileIndexLow:       info.FileIndexLow,
	}, nil
}

func removePinnedPrivateProxyHomeBasename(parent *os.Root, name string, want privateProxyFileIdentity) error {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reinspect private proxy home basename: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private proxy home basename was replaced during cleanup")
	}
	current, err := parent.OpenRoot(name)
	if err != nil {
		return fmt.Errorf("open current private proxy home basename: %w", err)
	}
	got, identityErr := privateProxyRootIdentity(current)
	closeErr := current.Close()
	if err := errors.Join(identityErr, closeErr); err != nil {
		return fmt.Errorf("identify current private proxy home basename: %w", err)
	}
	if got != want {
		return fmt.Errorf("private proxy home basename was replaced during cleanup")
	}
	if err := parent.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove private proxy home basename: %w", err)
	}
	return nil
}

func openPrivateProxyParent(path string) (*os.Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(absolute)
	if volume == "" {
		return nil, fmt.Errorf("private proxy home parent has no volume")
	}
	current := volume + string(filepath.Separator)
	relative := strings.TrimLeft(absolute[len(volume):], `\/`)
	for _, component := range strings.FieldsFunc(relative, func(r rune) bool {
		return r == '\\' || r == '/'
	}) {
		current = filepath.Join(current, component)
		pathUTF16, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return nil, err
		}
		attributes, err := windows.GetFileAttributes(pathUTF16)
		if err != nil {
			return nil, err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return nil, fmt.Errorf("private proxy home parent traverses reparse point %q", current)
		}
		if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return nil, fmt.Errorf("private proxy home parent component is not a directory: %q", current)
		}
	}
	if err := validatePrivateProxyParentSecurity(absolute); err != nil {
		return nil, err
	}
	return os.OpenRoot(absolute)
}

func validatePrivateProxyParentSecurity(path string) error {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		return err
	}
	if opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("private proxy home parent must be a regular directory")
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("inspect private proxy home parent ACL: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("inspect private proxy home parent owner: %w", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return fmt.Errorf("private proxy home parent must be owned by the current user")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("private proxy home parent must have a protected access list")
	}
	trustedWriteSIDs := []*windows.SID{user.User.Sid}
	for _, sidType := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinBuiltinAdministratorsSid,
		windows.WinLocalSystemSid,
	} {
		sid, sidErr := windows.CreateWellKnownSid(sidType)
		if sidErr != nil {
			return fmt.Errorf("inspect private proxy home parent ACL: %w", sidErr)
		}
		trustedWriteSIDs = append(trustedWriteSIDs, sid)
	}
	const fileDeleteChild = windows.ACCESS_MASK(0x00000040)
	writeMask := windows.ACCESS_MASK(
		windows.GENERIC_ALL|windows.GENERIC_WRITE|windows.FILE_WRITE_DATA|
			windows.FILE_APPEND_DATA|windows.FILE_WRITE_EA|windows.FILE_WRITE_ATTRIBUTES|
			windows.WRITE_DAC|windows.WRITE_OWNER|windows.DELETE,
	) | fileDeleteChild
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("inspect private proxy home parent ACL entry: %w", err)
		}
		if ace == nil || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask&writeMask == 0 {
			continue
		}
		standard, unsupported := privateProxyParentAllowACEType(ace.Header.AceType)
		if unsupported {
			return fmt.Errorf("private proxy home parent has unsupported allow ACE type %d", ace.Header.AceType)
		}
		if !standard {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trusted := false
		for _, trustedSID := range trustedWriteSIDs {
			if aceSID.Equals(trustedSID) {
				trusted = true
				break
			}
		}
		if !trusted {
			return fmt.Errorf("private proxy home parent grants write access to an untrusted principal")
		}
	}
	return nil
}

func privateProxyParentAllowACEType(aceType uint8) (standard, unsupported bool) {
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

func removePrivateProxyEntryInRoot(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %q: %w", name, err)
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			// os.Root.Chmod is only available in newer Go releases. Open the
			// file through the root and chmod the handle for Go 1.24 support.
			file, openErr := parent.OpenFile(name, os.O_WRONLY, 0)
			if openErr == nil {
				chmodErr := file.Chmod(info.Mode() | 0o200)
				closeErr := file.Close()
				if chmodErr != nil && !errors.Is(chmodErr, os.ErrNotExist) {
					return fmt.Errorf("make %q writable: %w", name, chmodErr)
				}
				if closeErr != nil && !errors.Is(closeErr, os.ErrNotExist) {
					return fmt.Errorf("close writable %q: %w", name, closeErr)
				}
			} else if !errors.Is(openErr, os.ErrNotExist) {
				return fmt.Errorf("open %q writable: %w", name, openErr)
			}
		}
		if err := parent.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %q: %w", name, err)
		}
		return nil
	}

	child, err := parent.OpenRoot(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open private proxy directory %q: %w", name, err)
	}
	contentsErr := removePrivateProxyRootContents(child)
	closeErr := child.Close()
	if err := errors.Join(contentsErr, closeErr); err != nil {
		return fmt.Errorf("remove private proxy directory %q: %w", name, err)
	}
	if err := parent.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove private proxy directory %q: %w", name, err)
	}
	return nil
}

func removePrivateProxyRootContents(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removePrivateProxyEntryInRoot(root, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}
