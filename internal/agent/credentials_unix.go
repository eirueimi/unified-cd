//go:build !windows

package agent

import (
	"fmt"
	"os"
)

func validateCredentialFile(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("credential file permissions must not allow group or other access")
	}
	return nil
}

func validateCredentialDirectory(_ string) error { return nil }

func protectCredentialFile(_ string, file *os.File) error {
	return file.Chmod(0o600)
}

func syncCredentialDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
