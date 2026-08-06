//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || netbsd || openbsd || solaris

package main

func notifyFrontMainPID(string) (bool, error) {
	return false, nil
}
