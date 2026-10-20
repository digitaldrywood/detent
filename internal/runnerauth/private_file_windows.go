package runnerauth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const fileFullControl = 0x1f01ff

func currentUserSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
}

func createPrivateFile(path string) (*os.File, error) {
	sd, err := currentUserSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func createPrivateTemp(directory, prefix string) (*os.File, error) {
	return createPrivateFile(filepath.Join(directory, prefix+rand.Text()))
}

func openPrivateFile(path string, forUpdate bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	if forUpdate {
		access |= windows.WRITE_DAC
	}
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("runner credential file is unavailable: %w", err)
	}
	file := os.NewFile(uintptr(handle), path)
	if forUpdate {
		sd, err := currentUserSecurityDescriptor()
		if err == nil {
			var dacl *windows.ACL
			dacl, _, err = sd.DACL()
			if err == nil {
				err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
			}
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("runner credential file permissions could not be secured: %w", err), file.Close())
		}
	}
	if err := validatePrivateDACL(handle); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func validatePrivateDACL(handle windows.Handle) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("runner credential file permissions could not be inspected: %w", err)
	}
	invalid := errors.New("runner credential file requires a protected DACL granting full control only to the current user; disable inheritance and remove other access entries with icacls before retrying")
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return invalid
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return invalid
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return invalid
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != fileFullControl {
		return invalid
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("runner credential file user could not be inspected: %w", err)
	}
	if !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(user.User.Sid) {
		return invalid
	}
	return nil
}
