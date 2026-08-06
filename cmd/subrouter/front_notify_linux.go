//go:build linux

package main

func notifyFrontMainPID(pid string) (bool, error) {
	return notifySystemdDescriptorStore("MAINPID="+pid, nil)
}
