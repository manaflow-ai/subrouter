//go:build windows

package cutovercanary

import "os"

func fileUID(os.FileInfo) int { return 0 }
