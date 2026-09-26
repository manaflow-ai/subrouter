//go:build linux

package main

func notifyFrontMainPID(pid string) (bool, error) {
	notified, err := notifySystemdDescriptorStore("MAINPID="+pid, nil)
	if err != nil || !notified {
		return notified, err
	}
	return notifySystemdBarrier(systemdNotifyBarrierTimeout)
}
