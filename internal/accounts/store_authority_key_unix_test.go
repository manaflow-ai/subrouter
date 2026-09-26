//go:build !windows

package accounts

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenPrivateStoreAuthorityKeyRejectsFIFOWithoutBlocking(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, storeAuthorityKeyFilename)
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("create authority-key FIFO: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		file, err := openPrivateStoreAuthorityKey(path)
		if file != nil {
			_ = file.Close()
		}
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("authority-key FIFO unexpectedly passed private-file validation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening an authority-key FIFO blocked")
	}
}
