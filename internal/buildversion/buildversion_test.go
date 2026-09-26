package buildversion

import (
	"runtime/debug"
	"strings"
	"testing"
)

func withStamp(t *testing.T, v, c, d string, bi *debug.BuildInfo) {
	t.Helper()
	oldV, oldC, oldD, oldRead := version, commit, buildDate, readBuildInfo
	version, commit, buildDate = v, c, d
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, bi != nil }
	t.Cleanup(func() { version, commit, buildDate, readBuildInfo = oldV, oldC, oldD, oldRead })
}

func TestGetPrefersLinkerStamp(t *testing.T) {
	withStamp(t, "v1.2.3", "0123456789abcdef0123", "2026-01-02T03:04:05Z", &debug.BuildInfo{
		Main:     debug.Module{Version: "v0.0.1"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "ffff"}, {Key: "vcs.modified", Value: "true"}},
	})
	info := Get()
	if info.Version != "v1.2.3" || info.Commit != "0123456789ab" || info.BuildDate != "2026-01-02T03:04:05Z" || info.Modified {
		t.Fatalf("Get() = %+v", info)
	}
	if got := Version(); got != "v1.2.3" {
		t.Fatalf("Version() = %q", got)
	}
}

func TestGetFallsBackToBuildInfo(t *testing.T) {
	withStamp(t, "", "", "", &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef0123456789"},
			{Key: "vcs.time", Value: "2026-09-01T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	info := Get()
	if info.Version != "devel" || info.Commit != "abcdef012345" || info.BuildDate != "2026-09-01T00:00:00Z" || !info.Modified {
		t.Fatalf("Get() = %+v", info)
	}
	if got := Version(); got != "devel+abcdef012345-dirty" {
		t.Fatalf("Version() = %q", got)
	}
	if s := info.String(); !strings.Contains(s, "abcdef012345-dirty") || !strings.HasPrefix(s, "devel ") {
		t.Fatalf("String() = %q", s)
	}
}

func TestGetWithoutAnyInfo(t *testing.T) {
	withStamp(t, "", "", "", nil)
	info := Get()
	if info.Version != "devel" || info.Commit != "unknown" || info.BuildDate != "unknown" {
		t.Fatalf("Get() = %+v", info)
	}
	if got := Version(); got != "devel" {
		t.Fatalf("Version() = %q", got)
	}
}

func TestModuleVersionUsedWhenUnstamped(t *testing.T) {
	withStamp(t, "", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.9.0"}})
	if got := Version(); got != "v0.9.0" {
		t.Fatalf("Version() = %q", got)
	}
}
