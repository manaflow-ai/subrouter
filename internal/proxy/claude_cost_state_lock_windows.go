//go:build windows

package proxy

// lockClaudeCostState is process-local on Windows: the in-process mutex in
// claudeCostStore serializes one worker, and Windows hosts do not run the
// supervisor's side-by-side canary.
func lockClaudeCostState(string) func() { return func() {} }
