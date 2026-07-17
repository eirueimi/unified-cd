//go:build windows

package agent

import (
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func readProtectedCredentialFile(path string) ([]byte, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(file)
	return data, info, err
}

const (
	fileAddFile         windows.ACCESS_MASK = 0x0002
	fileAddSubdirectory windows.ACCESS_MASK = 0x0004
	fileDeleteChild     windows.ACCESS_MASK = 0x0040
)

func validateCredentialFile(_ string, _ os.FileInfo) error { return nil }

// validateCredentialDirectory rejects inherited write grants to principals
// broader than the account running the agent. A broad inherited grant would
// let another account replace the temporary credential file before rename.
func validateCredentialDirectory(path string) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		return fmt.Errorf("credential directory ACL is not restricted")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("credential directory ACL is not restricted")
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return fmt.Errorf("credential directory ACL is not restricted")
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			continue
		}
		if !hasDirectoryWrite(ace.Mask) {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.IsWellKnown(windows.WinWorldSid) || sid.IsWellKnown(windows.WinAuthenticatedUserSid) || sid.IsWellKnown(windows.WinBuiltinUsersSid) || sid.IsWellKnown(windows.WinBuiltinGuestsSid) {
			return fmt.Errorf("credential directory ACL is not restricted")
		}
	}
	return nil
}

func hasDirectoryWrite(mask windows.ACCESS_MASK) bool {
	return mask&(windows.GENERIC_ALL|windows.GENERIC_WRITE|fileAddFile|fileAddSubdirectory|fileDeleteChild|windows.DELETE|windows.WRITE_DAC|windows.WRITE_OWNER) != 0
}

func protectCredentialFile(_ string, _ *os.File) error { return nil }

func syncCredentialDirectory(_ string) error { return nil }
