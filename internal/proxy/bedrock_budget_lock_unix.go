//go:build darwin || linux || freebsd || openbsd || netbsd

package proxy

import (
	"os"
	"syscall"
)

type bedrockBudgetLock struct{ f *os.File }

func lockBedrockBudget(path string) (*bedrockBudgetLock, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &bedrockBudgetLock{f}, nil
}
func (l *bedrockBudgetLock) Close() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
