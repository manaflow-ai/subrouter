//go:build darwin

package main

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// macOS browser cookie discovery for claude.ai session keys. Everything here
// is read-only: cookie databases are copied to a temp file before sqlite3
// opens them, and the only Keychain access is `security find-generic-password
// -w` for the browser's own "Safe Storage" entry (which may produce a
// one-time macOS permission prompt per browser).
//
// sessionKey values are secrets: they are never logged.

func init() {
	claudeWebDiscoverSessionKeys = discoverClaudeWebSessionKeys
}

// Test seams: tests must never touch the real Keychain or browser profiles.
var (
	claudeWebHomeDir    = os.UserHomeDir
	claudeWebRunCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}
)

type claudeWebChromiumBrowser struct {
	name            string
	profileRoot     string
	keychainService string
}

var claudeWebChromiumBrowsers = []claudeWebChromiumBrowser{
	{name: "Chrome", profileRoot: "Library/Application Support/Google/Chrome", keychainService: "Chrome Safe Storage"},
	{name: "Brave", profileRoot: "Library/Application Support/BraveSoftware/Brave-Browser", keychainService: "Brave Safe Storage"},
	{name: "Edge", profileRoot: "Library/Application Support/Microsoft Edge", keychainService: "Microsoft Edge Safe Storage"},
	{name: "Arc", profileRoot: "Library/Application Support/Arc/User Data", keychainService: "Arc Safe Storage"},
}

type claudeWebFirefoxFamily struct {
	name         string
	profilesRoot string
}

var claudeWebFirefoxFamilies = []claudeWebFirefoxFamily{
	{name: "Firefox", profilesRoot: "Library/Application Support/Firefox/Profiles"},
	{name: "Zen", profilesRoot: "Library/Application Support/zen/Profiles"},
}

func discoverClaudeWebSessionKeys(ctx context.Context) []claudeWebSessionKeyCandidate {
	home, err := claudeWebHomeDir()
	if err != nil {
		return nil
	}
	var out []claudeWebSessionKeyCandidate
	for _, browser := range claudeWebChromiumBrowsers {
		if ctx.Err() != nil {
			return out
		}
		out = append(out, chromiumClaudeSessionKeys(ctx, home, browser)...)
	}
	for _, family := range claudeWebFirefoxFamilies {
		if ctx.Err() != nil {
			return out
		}
		out = append(out, firefoxClaudeSessionKeys(ctx, home, family)...)
	}
	out = append(out, safariClaudeSessionKeys(home)...)
	return out
}

// Safari keeps its cookies in a binarycookies file. Reading it needs no
// decryption, but on machines without Full Disk Access the read simply fails
// and Safari falls out of the chain silently.
var claudeWebSafariCookieFiles = []string{
	"Library/Containers/com.apple.Safari/Data/Library/Cookies/Cookies.binarycookies",
	"Library/Cookies/Cookies.binarycookies",
}

func safariClaudeSessionKeys(home string) []claudeWebSessionKeyCandidate {
	var out []claudeWebSessionKeyCandidate
	for _, rel := range claudeWebSafariCookieFiles {
		data, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			continue
		}
		for _, key := range parseClaudeBinaryCookies(data) {
			out = append(out, claudeWebSessionKeyCandidate{SessionKey: key, Source: "Safari"})
		}
	}
	return out
}

func chromiumClaudeSessionKeys(ctx context.Context, home string, browser claudeWebChromiumBrowser) []claudeWebSessionKeyCandidate {
	dbs, _ := filepath.Glob(filepath.Join(home, browser.profileRoot, "*", "Cookies"))
	if len(dbs) == 0 {
		return nil
	}
	var key []byte
	var out []claudeWebSessionKeyCandidate
	for _, db := range dbs {
		if ctx.Err() != nil {
			return out
		}
		rows, err := claudeWebQuerySQLite(ctx, db,
			"SELECT hex(encrypted_value) FROM cookies WHERE name = 'sessionKey' AND host_key LIKE '%claude.ai'")
		if err != nil || len(rows) == 0 {
			continue
		}
		if key == nil {
			// Only touch the Keychain once a claude.ai cookie actually
			// exists, so sr status never prompts for a browser the user
			// does not use for Claude.
			password, err := claudeWebKeychainPassword(ctx, browser.keychainService)
			if err != nil {
				return out
			}
			key = pbkdf2SHA1([]byte(password), []byte("saltysalt"), 1003, 16)
		}
		for _, row := range rows {
			encrypted, err := hex.DecodeString(strings.TrimSpace(row))
			if err != nil {
				continue
			}
			value, err := decryptChromiumCookieValue(encrypted, key)
			if err != nil || !strings.HasPrefix(value, "sk-ant-") {
				continue
			}
			out = append(out, claudeWebSessionKeyCandidate{SessionKey: value, Source: browser.name})
			break
		}
	}
	return out
}

func firefoxClaudeSessionKeys(ctx context.Context, home string, family claudeWebFirefoxFamily) []claudeWebSessionKeyCandidate {
	dbs, _ := filepath.Glob(filepath.Join(home, family.profilesRoot, "*", "cookies.sqlite"))
	var out []claudeWebSessionKeyCandidate
	for _, db := range dbs {
		if ctx.Err() != nil {
			return out
		}
		rows, err := claudeWebQuerySQLite(ctx, db,
			"SELECT value FROM moz_cookies WHERE name = 'sessionKey' AND host LIKE '%claude.ai'")
		if err != nil {
			continue
		}
		for _, row := range rows {
			value := strings.TrimSpace(row)
			if !strings.HasPrefix(value, "sk-ant-") {
				continue
			}
			out = append(out, claudeWebSessionKeyCandidate{SessionKey: value, Source: family.name})
			break
		}
	}
	return out
}

func claudeWebKeychainPassword(ctx context.Context, service string) (string, error) {
	out, err := claudeWebRunCommand(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-w")
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// claudeWebQuerySQLite runs a read-only query against a temp copy of the
// database so a running browser's lock never interferes.
func claudeWebQuerySQLite(ctx context.Context, dbPath, query string) ([]string, error) {
	tmp, err := os.CreateTemp("", "sr-cookies-*.sqlite")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	data, err := os.ReadFile(dbPath)
	if err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	out, err := claudeWebRunCommand(ctx, "/usr/bin/sqlite3", "-readonly", tmpPath, query)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}
