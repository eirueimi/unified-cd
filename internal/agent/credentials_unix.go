//go:build !windows

package agent

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

func readProtectedCredentialFile(path string) ([]byte, os.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
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

func validateCredentialFile(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("credential file permissions must not allow group or other access")
	}
	return nil
}

func validateCredentialDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("credential directory permissions must not allow group or other writes")
	}
	return nil
}

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
