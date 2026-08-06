//go:build !linux

package main

func notifyFrontMainPID(string) (bool, error) {
	return false, nil
}
