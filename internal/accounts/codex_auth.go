package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// codexOAuthTokenURL is a var so tests can point refresh at a fake OAuth server
// that models the provider's rotate-on-use semantics. Nothing in production
// reassigns it.
var codexOAuthTokenURL = "https://auth.openai.com/oauth/token"

const codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
const codexAuthBreadcrumbLimit = 50

type codexRefreshReasonKey struct{}

func WithCodexRefreshReason(ctx context.Context, reason string) context.Context {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ctx
	}
	return context.WithValue(ctx, codexRefreshReasonKey{}, reason)
}

func CodexRefreshReason(ctx context.Context) string {
	reason, _ := ctx.Value(codexRefreshReasonKey{}).(string)
	return reason
}

func ReadActiveCodexAuth() (CodexAuthFile, bool, error) {
	return ReadCodexAuthFile(DefaultCodexAuthPath())
}

func ReadCodexAuthFile(path string) (CodexAuthFile, bool, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return CodexAuthFile{}, false, nil
	}
	if err != nil {
		return CodexAuthFile{}, false, err
	}
	var auth CodexAuthFile
	if err := json.Unmarshal(body, &auth); err != nil {
		return CodexAuthFile{}, false, err
	}
	return auth, true, nil
}

func WriteActiveCodexAuth(auth CodexAuthFile) error {
	body, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	return writeCodexActiveAuth(DefaultCodexAuthPath(), body)
}

func (s CodexStore) DetectActiveAccount() (string, error) {
	auth, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok {
		return "", err
	}
	if auth.Tokens != nil && auth.Tokens.IDToken != "" {
		email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
		if err == nil && email != "" {
			if _, found, err := s.FindStored(email); err != nil {
				return "", err
			} else if found {
				return email, nil
			}
		}
	}
	if auth.OpenAIAPIKey != "" {
		all, err := s.ListStored()
		if err != nil {
			return "", err
		}
		for _, account := range all {
			if account.Auth.OpenAIAPIKey == auth.OpenAIAPIKey {
				return account.Email, nil
			}
		}
	}
	return "", nil
}

func (s CodexStore) SyncActiveToStore() error {
	auth, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok {
		return err
	}
	if auth.Tokens == nil || auth.Tokens.IDToken == "" {
		return nil
	}
	email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || email == "" {
		return nil
	}
	lock, err := s.lockStoredAccount(email)
	if err != nil {
		return err
	}
	defer lock.Close()
	account, found, err := s.findStoredExact(email)
	if err != nil || !found {
		return err
	}
	if accountAuthNewerThanIncoming(account.Auth, auth) {
		logCodexAuthStoreSkipped("codex oauth active auth sync skipped", s, account, "stored_auth_newer")
		return nil
	}
	previous := account
	account.Auth = auth
	appendCodexAuthBreadcrumb(context.Background(), s, &account, "active_auth_synced", "active_auth", false, &previous, &account, nil, nil)
	if err := s.saveStoredUnlocked(account); err != nil {
		return err
	}
	logCodexAuthStoreUpdated("codex oauth active auth synced", s, previous, account, "active_auth")
	return nil
}

func (s CodexStore) ImportActive() (StoredCodexAccount, bool, error) {
	auth, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok {
		return StoredCodexAccount{}, false, err
	}
	if auth.Tokens == nil || auth.Tokens.IDToken == "" {
		return StoredCodexAccount{}, false, fmt.Errorf("no active Codex OAuth auth found in %s", DefaultCodexAuthPath())
	}
	email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || email == "" {
		return StoredCodexAccount{}, false, fmt.Errorf("could not extract email from current auth token")
	}
	account, existed, err := s.FindStored(email)
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	if !existed {
		account = StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	previous := account
	account.Auth = auth
	appendCodexAuthBreadcrumb(context.Background(), s, &account, "active_auth_imported", "active_auth", false, &previous, &account, nil, nil)
	err = s.SaveStored(account)
	if err == nil {
		logCodexAuthStoreUpdated("codex oauth active auth imported", s, previous, account, "active_auth")
	}
	return account, existed, err
}

func (s CodexStore) AddAPIKey(label, key string) (StoredCodexAccount, bool, error) {
	label = strings.TrimSpace(label)
	key = strings.TrimSpace(key)
	if label == "" {
		return StoredCodexAccount{}, false, fmt.Errorf("label is required")
	}
	if !strings.HasPrefix(key, "sk-") {
		return StoredCodexAccount{}, false, fmt.Errorf("invalid API key format, expected sk-...")
	}
	email := "apikey:" + label
	account, existed, err := s.FindStored(email)
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	if !existed {
		account = StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	account.Auth = CodexAuthFile{
		AuthMode:     "apikey",
		OpenAIAPIKey: key,
	}
	return account, existed, s.SaveStored(account)
}

func (s CodexStore) RefreshStoredIfExpired(ctx context.Context, client *http.Client, account StoredCodexAccount) (StoredCodexAccount, bool, error) {
	return s.refreshStored(ctx, client, account, false)
}

func (s CodexStore) RefreshStored(ctx context.Context, client *http.Client, account StoredCodexAccount) (StoredCodexAccount, bool, error) {
	return s.refreshStored(ctx, client, account, true)
}

func (s CodexStore) refreshStored(ctx context.Context, client *http.Client, account StoredCodexAccount, force bool) (StoredCodexAccount, bool, error) {
	if account.Auth.Tokens == nil {
		logCodexRefreshSkipped(ctx, s, account, force, "missing_tokens")
		return account, false, nil
	}
	if !force && !IsJWTExpired(account.Auth.Tokens.AccessToken, 60*time.Second) {
		logCodexRefreshSkipped(ctx, s, account, force, "access_token_fresh")
		return account, false, nil
	}
	if err := terminalStoredRefreshFailure(account); err != nil {
		logCodexRefreshSkipped(ctx, s, account, force, "terminal_refresh_failure")
		return account, false, err
	}

	lock, err := s.lockStoredAccount(account.Email)
	if err != nil {
		logCodexRefreshFailed(ctx, s, account, force, err)
		return account, false, err
	}
	defer lock.Close()

	latest, found, err := s.findStoredExact(account.Email)
	if err != nil {
		logCodexRefreshFailed(ctx, s, account, force, err)
		return account, false, err
	}
	if found {
		account = latest
	}
	if account.Auth.Tokens == nil {
		logCodexRefreshSkipped(ctx, s, account, force, "missing_tokens_after_lock")
		return account, false, nil
	}
	if !force && !IsJWTExpired(account.Auth.Tokens.AccessToken, 60*time.Second) {
		logCodexRefreshSkipped(ctx, s, account, force, "access_token_fresh_after_lock")
		return account, false, nil
	}
	if err := terminalStoredRefreshFailure(account); err != nil {
		logCodexRefreshSkipped(ctx, s, account, force, "terminal_refresh_failure_after_lock")
		return account, false, err
	}

	previous := account
	logCodexRefreshStart(ctx, s, previous, force)
	auth, err := RefreshCodexAuth(ctx, client, account.Auth)
	if err != nil {
		if recovered, ok := s.recoverRefreshedAccount(account); ok {
			appendCodexAuthBreadcrumb(ctx, s, &recovered, "refresh_recovered_concurrent_update", "oauth_refresh", force, &previous, nil, &recovered, err)
			if saveErr := s.saveStoredUnlocked(recovered); saveErr != nil {
				logCodexRefreshFailed(ctx, s, recovered, force, saveErr)
			}
			logCodexRefreshRecovered(ctx, s, account, recovered, force, err)
			return recovered, false, nil
		}
		if failure, ok := codexRefreshFailureFromError(err); ok {
			account.Auth.RefreshFailure = failure
			appendCodexAuthBreadcrumb(ctx, s, &account, "refresh_terminal_failure", "oauth_refresh", force, &previous, nil, nil, err)
			if saveErr := s.saveStoredUnlocked(account); saveErr != nil {
				logCodexRefreshFailed(ctx, s, account, force, saveErr)
			}
		}
		logCodexRefreshFailed(ctx, s, account, force, err)
		return account, false, err
	}
	auth.RefreshFailure = nil
	account.Auth = auth
	appendCodexAuthBreadcrumb(ctx, s, &account, "refresh_succeeded", "oauth_refresh", force, &previous, &account, nil, nil)
	if err := s.saveStoredUnlocked(account); err != nil {
		logCodexRefreshFailed(ctx, s, account, force, err)
		return account, true, err
	}
	if err := syncActiveCodexAuthIfAccountActive(account); err != nil {
		logCodexRefreshFailed(ctx, s, account, force, err)
		return account, true, err
	}
	logCodexRefreshSucceeded(ctx, s, previous, account, force)
	return account, true, nil
}

func (s CodexStore) recoverRefreshedAccount(previous StoredCodexAccount) (StoredCodexAccount, bool) {
	latest, found, err := s.findStoredExact(previous.Email)
	if err != nil || !found || latest.Auth.Tokens == nil {
		return StoredCodexAccount{}, false
	}
	if previous.Auth.Tokens != nil &&
		latest.Auth.Tokens.AccessToken == previous.Auth.Tokens.AccessToken &&
		latest.Auth.Tokens.RefreshToken == previous.Auth.Tokens.RefreshToken {
		return StoredCodexAccount{}, false
	}
	if IsJWTExpired(latest.Auth.Tokens.AccessToken, 60*time.Second) {
		return StoredCodexAccount{}, false
	}
	return latest, true
}

type CodexStoredRefreshFailureError struct {
	Failure CodexRefreshFailure
}

func (e *CodexStoredRefreshFailureError) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{"token refresh previously failed"}
	if e.Failure.StatusCode != 0 {
		parts[0] = fmt.Sprintf("%s (%d)", parts[0], e.Failure.StatusCode)
	}
	if e.Failure.ProviderCode != "" {
		parts = append(parts, e.Failure.ProviderCode)
	}
	if e.Failure.ProviderMessage != "" {
		parts = append(parts, e.Failure.ProviderMessage)
	}
	if e.Failure.At != "" {
		parts = append(parts, "reauth required; recorded at "+e.Failure.At)
	} else {
		parts = append(parts, "reauth required")
	}
	return strings.Join(parts, ": ")
}

func terminalStoredRefreshFailure(account StoredCodexAccount) error {
	failure := account.Auth.RefreshFailure
	if failure == nil || !isTerminalCodexRefreshFailure(failure.StatusCode, failure.ProviderCode, failure.ProviderMessage) {
		return nil
	}
	return &CodexStoredRefreshFailureError{Failure: *failure}
}

func codexRefreshFailureFromError(err error) (*CodexRefreshFailure, bool) {
	var refreshErr *CodexAuthRefreshError
	if !errors.As(err, &refreshErr) ||
		!isTerminalCodexRefreshFailure(refreshErr.StatusCode, refreshErr.ProviderCode, refreshErr.ProviderMessage) {
		return nil, false
	}
	return &CodexRefreshFailure{
		At:              time.Now().UTC().Format(time.RFC3339),
		StatusCode:      refreshErr.StatusCode,
		ProviderType:    refreshErr.ProviderType,
		ProviderCode:    refreshErr.ProviderCode,
		ProviderMessage: refreshErr.ProviderMessage,
	}, true
}

func isTerminalCodexRefreshFailure(statusCode int, providerCode, providerMessage string) bool {
	providerCode = strings.ToLower(strings.TrimSpace(providerCode))
	providerMessage = strings.ToLower(providerMessage)
	// Codex emits this locally when auth.json flipped to another account mid-refresh.
	if strings.Contains(providerMessage, "logged out or signed in to another account") ||
		strings.Contains(providerMessage, "access token could not be refreshed because you have since") {
		return true
	}
	if statusCode != http.StatusBadRequest && statusCode != http.StatusUnauthorized {
		return false
	}
	return strings.Contains(providerCode, "refresh_token") || strings.Contains(providerMessage, "refresh token")
}

func accountAuthNewerThanIncoming(stored, incoming CodexAuthFile) bool {
	if stored.Tokens == nil || incoming.Tokens == nil {
		return false
	}
	if stored.Tokens.AccessToken == incoming.Tokens.AccessToken &&
		stored.Tokens.RefreshToken == incoming.Tokens.RefreshToken {
		return false
	}
	storedExpiry, storedOK := JWTExpiryMillis(stored.Tokens.AccessToken)
	incomingExpiry, incomingOK := JWTExpiryMillis(incoming.Tokens.AccessToken)
	if storedOK && incomingOK {
		return storedExpiry > incomingExpiry
	}
	return !IsJWTExpired(stored.Tokens.AccessToken, 60*time.Second) &&
		IsJWTExpired(incoming.Tokens.AccessToken, 60*time.Second)
}

func syncActiveCodexAuthIfAccountActive(account StoredCodexAccount) error {
	activeEmail, ok, err := activeCodexAuthEmail()
	if err != nil || !ok || activeEmail != account.Email {
		return err
	}
	return WriteActiveCodexAuth(account.Auth)
}

func activeCodexAuthEmail() (string, bool, error) {
	auth, ok, err := ReadActiveCodexAuth()
	if err != nil || !ok || auth.Tokens == nil || auth.Tokens.IDToken == "" {
		return "", false, err
	}
	email, err := ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || email == "" {
		return "", false, err
	}
	return email, true, nil
}

func RefreshCodexAuthIfExpired(ctx context.Context, client *http.Client, auth CodexAuthFile) (CodexAuthFile, bool, error) {
	if auth.Tokens == nil {
		return auth, false, nil
	}
	if !IsJWTExpired(auth.Tokens.AccessToken, 60*time.Second) {
		return auth, false, nil
	}
	refreshed, err := RefreshCodexAuth(ctx, client, auth)
	return refreshed, err == nil, err
}

func RefreshCodexAuth(ctx context.Context, client *http.Client, auth CodexAuthFile) (CodexAuthFile, error) {
	auth = cloneCodexAuthFile(auth)
	if auth.Tokens == nil {
		return auth, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	payload := map[string]string{
		"client_id":     codexOAuthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": auth.Tokens.RefreshToken,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return auth, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, bytes.NewReader(body))
	if err != nil {
		return auth, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return auth, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		return auth, newCodexAuthRefreshError(res.StatusCode, strings.TrimSpace(buf.String()))
	}
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&refreshed); err != nil {
		return auth, err
	}
	if refreshed.AccessToken == "" || refreshed.RefreshToken == "" || refreshed.IDToken == "" {
		return auth, fmt.Errorf("token refresh response missing required fields")
	}
	auth.Tokens.AccessToken = refreshed.AccessToken
	auth.Tokens.RefreshToken = refreshed.RefreshToken
	auth.Tokens.IDToken = refreshed.IDToken
	auth.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	return auth, nil
}

func cloneCodexAuthFile(auth CodexAuthFile) CodexAuthFile {
	if auth.Tokens != nil {
		tokens := *auth.Tokens
		auth.Tokens = &tokens
	}
	if auth.RefreshFailure != nil {
		failure := *auth.RefreshFailure
		auth.RefreshFailure = &failure
	}
	return auth
}

type CodexAuthRefreshError struct {
	StatusCode      int
	Body            string
	ProviderType    string
	ProviderCode    string
	ProviderMessage string
}

func newCodexAuthRefreshError(statusCode int, body string) *CodexAuthRefreshError {
	err := &CodexAuthRefreshError{
		StatusCode: statusCode,
		Body:       body,
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &payload) == nil {
		err.ProviderMessage = payload.Error.Message
		err.ProviderType = payload.Error.Type
		err.ProviderCode = payload.Error.Code
	}
	return err
}

func (e *CodexAuthRefreshError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("token refresh failed (%d)", e.StatusCode)
	}
	return fmt.Sprintf("token refresh failed (%d): %s", e.StatusCode, e.Body)
}

func logCodexRefreshStart(ctx context.Context, store CodexStore, account StoredCodexAccount, force bool) {
	slog.Info("codex oauth refresh start", codexRefreshLogAttrs(ctx, store, account, force)...)
}

func logCodexRefreshSucceeded(ctx context.Context, store CodexStore, previous, account StoredCodexAccount, force bool) {
	attrs := codexRefreshLogAttrs(ctx, store, previous, force)
	attrs = append(attrs,
		"old_refresh_fp", codexRefreshFingerprintForLog(previous),
		"new_access_exp", codexAccessExpiryForLog(account),
		"new_access_fp", codexAccessFingerprintForLog(account),
		"new_refresh_fp", codexRefreshFingerprintForLog(account),
		"new_account_id", codexAccountIDForLog(account),
	)
	slog.Info("codex oauth refresh succeeded", attrs...)
}

func logCodexRefreshRecovered(ctx context.Context, store CodexStore, previous, recovered StoredCodexAccount, force bool, err error) {
	attrs := codexRefreshLogAttrs(ctx, store, previous, force)
	attrs = append(attrs,
		"recovered_access_exp", codexAccessExpiryForLog(recovered),
		"recovered_access_fp", codexAccessFingerprintForLog(recovered),
		"recovered_refresh_fp", codexRefreshFingerprintForLog(recovered),
		"recovered_account_id", codexAccountIDForLog(recovered),
	)
	appendCodexRefreshErrorAttrs(&attrs, err)
	slog.Warn("codex oauth refresh recovered from concurrent update", attrs...)
}

func logCodexRefreshFailed(ctx context.Context, store CodexStore, account StoredCodexAccount, force bool, err error) {
	attrs := codexRefreshLogAttrs(ctx, store, account, force)
	appendCodexRefreshErrorAttrs(&attrs, err)
	slog.Warn("codex oauth refresh failed", attrs...)
}

func logCodexRefreshSkipped(ctx context.Context, store CodexStore, account StoredCodexAccount, force bool, reason string) {
	attrs := codexRefreshLogAttrs(ctx, store, account, force)
	attrs = append(attrs, "skip_reason", reason)
	slog.Debug("codex oauth refresh skipped", attrs...)
}

func codexRefreshLogAttrs(ctx context.Context, store CodexStore, account StoredCodexAccount, force bool) []any {
	reason := CodexRefreshReason(ctx)
	if reason == "" {
		reason = "unspecified"
	}
	attrs := codexAuthCorrelationAttrs(store, account)
	return append(attrs,
		"account", account.Email,
		"reason", reason,
		"force", force,
		"last_refresh", account.Auth.LastRefresh,
		"access_exp", codexAccessExpiryForLog(account),
		"access_expired", codexAccessExpiredForLog(account),
	)
}

func logCodexAuthStoreUpdated(message string, store CodexStore, previous, account StoredCodexAccount, source string) {
	attrs := codexAuthCorrelationAttrs(store, account)
	attrs = append(attrs,
		"account", account.Email,
		"source", source,
		"last_refresh", account.Auth.LastRefresh,
		"access_exp", codexAccessExpiryForLog(account),
	)
	if previous.Auth.Tokens != nil {
		attrs = append(attrs,
			"previous_access_fp", codexAccessFingerprintForLog(previous),
			"previous_refresh_fp", codexRefreshFingerprintForLog(previous),
			"previous_account_id", codexAccountIDForLog(previous),
		)
	}
	slog.Info(message, attrs...)
}

func logCodexAuthStoreSkipped(message string, store CodexStore, account StoredCodexAccount, reason string) {
	attrs := codexAuthCorrelationAttrs(store, account)
	attrs = append(attrs,
		"account", account.Email,
		"skip_reason", reason,
		"last_refresh", account.Auth.LastRefresh,
		"access_exp", codexAccessExpiryForLog(account),
	)
	slog.Debug(message, attrs...)
}

func appendCodexAuthBreadcrumb(ctx context.Context, store CodexStore, account *StoredCodexAccount, event, source string, force bool, old, next, recovered *StoredCodexAccount, err error) {
	if account == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		event = "unknown"
	}
	host, _ := os.Hostname()
	executable, _ := os.Executable()
	workingDir, _ := os.Getwd()
	crumb := CodexAuthBreadcrumb{
		At:            time.Now().UTC().Format(time.RFC3339Nano),
		Event:         event,
		Source:        strings.TrimSpace(source),
		Reason:        codexRefreshReasonOrUnspecified(ctx),
		Host:          host,
		PID:           os.Getpid(),
		PPID:          os.Getppid(),
		Executable:    executable,
		WorkingDir:    workingDir,
		StoreDir:      store.Dir,
		SourcePath:    account.SourcePath(store),
		Force:         force,
		LastRefresh:   account.Auth.LastRefresh,
		AccessExp:     codexAccessExpiryForLog(*account),
		AccessExpired: codexAccessExpiredForLog(*account),
		AccessFP:      codexAccessFingerprintForLog(*account),
		RefreshFP:     codexRefreshFingerprintForLog(*account),
		AccountID:     codexAccountIDForLog(*account),
	}
	if old != nil && old.Auth.Tokens != nil {
		crumb.OldAccessExp = codexAccessExpiryForLog(*old)
		crumb.OldAccessFP = codexAccessFingerprintForLog(*old)
		crumb.OldRefreshFP = codexRefreshFingerprintForLog(*old)
		crumb.OldAccountID = codexAccountIDForLog(*old)
	}
	if next != nil && next.Auth.Tokens != nil {
		crumb.NewAccessExp = codexAccessExpiryForLog(*next)
		crumb.NewAccessFP = codexAccessFingerprintForLog(*next)
		crumb.NewRefreshFP = codexRefreshFingerprintForLog(*next)
		crumb.NewAccountID = codexAccountIDForLog(*next)
	}
	if recovered != nil && recovered.Auth.Tokens != nil {
		crumb.RecoveredAccessExp = codexAccessExpiryForLog(*recovered)
		crumb.RecoveredAccessFP = codexAccessFingerprintForLog(*recovered)
		crumb.RecoveredRefreshFP = codexRefreshFingerprintForLog(*recovered)
		crumb.RecoveredAccountID = codexAccountIDForLog(*recovered)
	}
	appendCodexRefreshErrorToBreadcrumb(&crumb, err)

	account.Breadcrumbs = append(account.Breadcrumbs, crumb)
	if len(account.Breadcrumbs) > codexAuthBreadcrumbLimit {
		account.Breadcrumbs = account.Breadcrumbs[len(account.Breadcrumbs)-codexAuthBreadcrumbLimit:]
	}
}

func codexRefreshReasonOrUnspecified(ctx context.Context) string {
	if ctx == nil {
		return "unspecified"
	}
	reason := CodexRefreshReason(ctx)
	if strings.TrimSpace(reason) == "" {
		return "unspecified"
	}
	return reason
}

func appendCodexRefreshErrorToBreadcrumb(crumb *CodexAuthBreadcrumb, err error) {
	if crumb == nil || err == nil {
		return
	}
	var refreshErr *CodexAuthRefreshError
	if !errors.As(err, &refreshErr) {
		return
	}
	crumb.StatusCode = refreshErr.StatusCode
	crumb.ProviderType = refreshErr.ProviderType
	crumb.ProviderCode = refreshErr.ProviderCode
	crumb.ProviderMessage = refreshErr.ProviderMessage
}

func codexAuthCorrelationAttrs(store CodexStore, account StoredCodexAccount) []any {
	host, _ := os.Hostname()
	return []any{
		"host", host,
		"pid", os.Getpid(),
		"store_dir", store.Dir,
		"source_path", account.SourcePath(store),
		"access_fp", codexAccessFingerprintForLog(account),
		"refresh_fp", codexRefreshFingerprintForLog(account),
		"account_id", codexAccountIDForLog(account),
	}
}

func codexAccessExpiryForLog(account StoredCodexAccount) string {
	if account.Auth.Tokens == nil {
		return ""
	}
	expMillis, ok := JWTExpiryMillis(account.Auth.Tokens.AccessToken)
	if !ok {
		return ""
	}
	return time.UnixMilli(expMillis).UTC().Format(time.RFC3339)
}

func codexAccessExpiredForLog(account StoredCodexAccount) bool {
	if account.Auth.Tokens == nil {
		return false
	}
	return IsJWTExpired(account.Auth.Tokens.AccessToken, 60*time.Second)
}

func codexAccessFingerprintForLog(account StoredCodexAccount) string {
	if account.Auth.Tokens == nil {
		return ""
	}
	return codexTokenFingerprint(account.Auth.Tokens.AccessToken)
}

func codexRefreshFingerprintForLog(account StoredCodexAccount) string {
	if account.Auth.Tokens == nil {
		return ""
	}
	return codexTokenFingerprint(account.Auth.Tokens.RefreshToken)
}

func codexAccountIDForLog(account StoredCodexAccount) string {
	if account.Auth.Tokens == nil {
		return ""
	}
	return account.Auth.Tokens.AccountID
}

func codexTokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

func appendCodexRefreshErrorAttrs(attrs *[]any, err error) {
	if err == nil {
		return
	}
	var refreshErr *CodexAuthRefreshError
	if errors.As(err, &refreshErr) {
		*attrs = append(*attrs,
			"status", refreshErr.StatusCode,
			"provider_type", refreshErr.ProviderType,
			"provider_code", refreshErr.ProviderCode,
			"provider_message", refreshErr.ProviderMessage,
		)
		return
	}
	*attrs = append(*attrs, "error", err)
}

func DecodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func ExtractEmailFromJWT(token string) (string, error) {
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return "", err
	}
	if profile, ok := claims["https://api.openai.com/profile"].(map[string]any); ok {
		if email, ok := profile["email"].(string); ok && email != "" {
			return email, nil
		}
	}
	if email, ok := claims["email"].(string); ok {
		return email, nil
	}
	return "", nil
}

func ExtractChatGPTAccountID(auth CodexAuthFile) string {
	if auth.Tokens == nil {
		return ""
	}
	if auth.Tokens.AccountID != "" {
		return auth.Tokens.AccountID
	}
	for _, token := range []string{auth.Tokens.IDToken, auth.Tokens.AccessToken} {
		accountID := ExtractChatGPTAccountIDFromJWT(token)
		if accountID != "" {
			return accountID
		}
	}
	return ""
}

func ExtractChatGPTAccountIDFromJWT(token string) string {
	if token == "" {
		return ""
	}
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return ""
	}
	if accountID, ok := claims["chatgpt_account_id"].(string); ok && accountID != "" {
		return accountID
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if accountID, ok := auth["chatgpt_account_id"].(string); ok && accountID != "" {
			return accountID
		}
	}
	if orgs, ok := claims["organizations"].([]any); ok && len(orgs) > 0 {
		if org, ok := orgs[0].(map[string]any); ok {
			if id, ok := org["id"].(string); ok && id != "" {
				return id
			}
		}
	}
	return ""
}

func JWTExpiryMillis(token string) (int64, bool) {
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return 0, false
	}
	exp, ok := numericClaim(claims["exp"])
	if !ok {
		return 0, false
	}
	return int64(exp * 1000), true
}

func IsJWTExpired(token string, grace time.Duration) bool {
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return false
	}
	exp, ok := numericClaim(claims["exp"])
	if !ok {
		return false
	}
	return time.Until(time.Unix(int64(exp), 0)) < grace
}

func numericClaim(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case json.Number:
		out, err := v.Float64()
		return out, err == nil
	default:
		return 0, false
	}
}
