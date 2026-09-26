//go:build !darwin

package main

import "context"

// Browser cookie reading is macOS-only; everywhere else discovery is a no-op
// and `sr status` rows render exactly as the server/OAuth data produced them.
func init() {
	claudeWebDiscoverSessionKeys = func(context.Context) []claudeWebSessionKeyCandidate {
		return nil
	}
}
