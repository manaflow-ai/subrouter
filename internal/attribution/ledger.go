// Package attribution keeps a small, append-only ledger of who sent which
// prompt through the gateway, so an operator can answer "what did each person
// ask their agents to do" without reading raw transcripts.
//
// The person is the tailnet peer behind the connection, as asserted by the
// gateway's own tailscaled. A header the client sets (X-Subrouter-User-Email)
// is kept only as ClaimedUser: it is never used as the person, because anything
// that reaches the port can set it.
//
// Recording is best effort and off the request path: Submit never blocks,
// never fails a request, and drops (and counts) work when the queue is full.
package attribution

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// Unattributed is the person for a peer the tailnet could not name.
	Unattributed = "unattributed"
	// MaxPromptChars bounds the stored prompt text per entry.
	MaxPromptChars = 4000
	// DefaultRetention deletes ledger days older than this.
	DefaultRetention = 30 * 24 * time.Hour
	// DefaultQueryLimit and MaxQueryLimit bound one query's result.
	DefaultQueryLimit = 500
	MaxQueryLimit     = 5000

	defaultQueueSize  = 1024
	dedupeSessions    = 10000
	identifyTimeout   = 3 * time.Second
	pruneInterval     = time.Hour
	ledgerFileSuffix  = ".jsonl"
	ledgerDateLayout  = "2006-01-02"
	systemReminderTag = "<system-reminder>"
)

// Identity is the tailnet principal behind a connection.
type Identity struct {
	LoginName string
	NodeName  string
	Tags      []string
}

// IdentifyFunc resolves a remote address to its tailnet identity.
type IdentifyFunc func(ctx context.Context, remoteAddr string) (Identity, bool)

// Request is what the proxy knows about a request when it submits it.
type Request struct {
	RemoteAddr      string
	Agent           string
	SessionID       string
	ClientSessionID string
	Model           string
	Path            string
	ClaimedUser     string
	UserAgent       string
}

// Entry is one ledger line.
type Entry struct {
	Time            time.Time `json:"time"`
	Person          string    `json:"person"`
	Node            string    `json:"node,omitempty"`
	Tags            []string  `json:"tags,omitempty"`
	ClaimedUser     string    `json:"claimed_user,omitempty"`
	Agent           string    `json:"agent,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	ClientSessionID string    `json:"client_session_id,omitempty"`
	Model           string    `json:"model,omitempty"`
	Path            string    `json:"path,omitempty"`
	UserAgent       string    `json:"user_agent,omitempty"`
	BodyBytes       int       `json:"body_bytes"`
	PromptSHA256    string    `json:"prompt_sha256"`
	Prompt          string    `json:"prompt"`
	PromptTruncated bool      `json:"prompt_truncated,omitempty"`
}

// Options configures a ledger. Zero values take the defaults.
type Options struct {
	Identify  IdentifyFunc
	Retention time.Duration
	QueueSize int
	Now       func() time.Time
}

// Stats is reported on /_subrouter/health.
type Stats struct {
	Recorded     uint64 `json:"recorded"`
	Unattributed uint64 `json:"unattributed"`
	Deduped      uint64 `json:"deduped"`
	Skipped      uint64 `json:"skipped_non_prompt"`
	DroppedQueue uint64 `json:"dropped_queue_full"`
	WriteErrors  uint64 `json:"write_errors"`
	RetentionH   int    `json:"retention_hours"`
}

type job struct {
	request Request
	body    []byte
	flush   chan struct{}
}

// Ledger writes one JSONL file per UTC day under dir, mode 0600.
type Ledger struct {
	dir       string
	identify  IdentifyFunc
	retention time.Duration
	now       func() time.Time

	queue chan job
	done  chan struct{}
	once  sync.Once

	// worker-owned state
	lastPrompt map[string]string
	prunedAt   time.Time

	mu sync.Mutex // serializes file writes against Prune and Query

	recorded, unattributed, deduped, skipped, droppedQueue, writeErrors atomic.Uint64
}

// Open creates dir (0700) and starts the background writer.
func Open(dir string, options Options) (*Ledger, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("prompt ledger directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create prompt ledger dir: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	if options.Retention <= 0 {
		options.Retention = DefaultRetention
	}
	if options.QueueSize <= 0 {
		options.QueueSize = defaultQueueSize
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	l := &Ledger{
		dir:        dir,
		identify:   options.Identify,
		retention:  options.Retention,
		now:        options.Now,
		queue:      make(chan job, options.QueueSize),
		done:       make(chan struct{}),
		lastPrompt: map[string]string{},
	}
	go l.run()
	return l, nil
}

// Dir is where the ledger lives.
func (l *Ledger) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// Submit queues a request body for attribution. It never blocks. The caller
// must not modify body afterwards.
func (l *Ledger) Submit(request Request, body []byte) {
	if l == nil || len(body) == 0 {
		return
	}
	select {
	case l.queue <- job{request: request, body: body}:
	default:
		l.droppedQueue.Add(1)
	}
}

// Flush waits until every job queued before it is written.
func (l *Ledger) Flush() {
	if l == nil {
		return
	}
	ch := make(chan struct{})
	select {
	case l.queue <- job{flush: ch}:
		<-ch
	case <-l.done:
	}
}

// Close stops the writer after draining queued jobs.
func (l *Ledger) Close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.Flush()
		close(l.done)
	})
}

// Stats returns the counters.
func (l *Ledger) Stats() Stats {
	if l == nil {
		return Stats{}
	}
	return Stats{
		Recorded:     l.recorded.Load(),
		Unattributed: l.unattributed.Load(),
		Deduped:      l.deduped.Load(),
		Skipped:      l.skipped.Load(),
		DroppedQueue: l.droppedQueue.Load(),
		WriteErrors:  l.writeErrors.Load(),
		RetentionH:   int(l.retention / time.Hour),
	}
}

func (l *Ledger) run() {
	for {
		select {
		case j := <-l.queue:
			if j.flush != nil {
				close(j.flush)
				continue
			}
			l.handle(j)
		case <-l.done:
			return
		}
	}
}

func (l *Ledger) handle(j job) {
	text, ok := ExtractPrompt(j.body)
	if !ok {
		l.skipped.Add(1)
		return
	}
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])
	key := j.request.Agent + "\x00" + j.request.SessionID
	if j.request.SessionID != "" {
		if l.lastPrompt[key] == hash {
			l.deduped.Add(1)
			return
		}
		if len(l.lastPrompt) >= dedupeSessions {
			l.lastPrompt = map[string]string{}
		}
		l.lastPrompt[key] = hash
	}

	identity, identified := Identity{}, false
	if l.identify != nil {
		ctx, cancel := context.WithTimeout(context.Background(), identifyTimeout)
		identity, identified = l.identify(ctx, j.request.RemoteAddr)
		cancel()
	}
	person := PersonFor(identity, identified)
	if person == Unattributed {
		l.unattributed.Add(1)
	}

	truncated := false
	if utf8.RuneCountInString(text) > MaxPromptChars {
		text = string([]rune(text)[:MaxPromptChars])
		truncated = true
	}
	entry := Entry{
		Time:            l.now().UTC(),
		Person:          person,
		Node:            identity.NodeName,
		Tags:            identity.Tags,
		ClaimedUser:     j.request.ClaimedUser,
		Agent:           j.request.Agent,
		SessionID:       j.request.SessionID,
		ClientSessionID: j.request.ClientSessionID,
		Model:           j.request.Model,
		Path:            j.request.Path,
		UserAgent:       j.request.UserAgent,
		BodyBytes:       len(j.body),
		PromptSHA256:    hash,
		Prompt:          text,
		PromptTruncated: truncated,
	}
	if err := l.write(entry); err != nil {
		l.writeErrors.Add(1)
		return
	}
	l.recorded.Add(1)
	if l.now().Sub(l.prunedAt) >= pruneInterval {
		l.Prune()
	}
}

func (l *Ledger) write(entry Entry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	path := filepath.Join(l.dir, entry.Time.Format(ledgerDateLayout)+ledgerFileSuffix)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(line)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// Prune deletes day files whose whole day is older than the retention.
func (l *Ledger) Prune() {
	if l == nil {
		return
	}
	now := l.now().UTC()
	l.prunedAt = now
	cutoff := now.Add(-l.retention)
	l.mu.Lock()
	defer l.mu.Unlock()
	names, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, name := range names {
		day, ok := dayOf(name.Name())
		if !ok {
			continue
		}
		if day.Add(24 * time.Hour).Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, name.Name()))
		}
	}
}

func dayOf(name string) (time.Time, bool) {
	if !strings.HasSuffix(name, ledgerFileSuffix) {
		return time.Time{}, false
	}
	day, err := time.Parse(ledgerDateLayout, strings.TrimSuffix(name, ledgerFileSuffix))
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// Filter selects entries. Person matches the full login case-insensitively,
// or its local part ("leo" matches "leo@manaflow.ai"), or a tag.
type Filter struct {
	Person string
	Since  time.Time
	Until  time.Time
	Limit  int
}

// Matches reports whether person satisfies the filter's person.
func (f Filter) Matches(person string) bool {
	want := strings.ToLower(strings.TrimSpace(f.Person))
	if want == "" {
		return true
	}
	got := strings.ToLower(person)
	if got == want {
		return true
	}
	if local, _, ok := strings.Cut(got, "@"); ok && !strings.Contains(want, "@") && local == want {
		return true
	}
	return false
}

// Query returns matching entries, oldest first, at most Limit (newest kept).
func (l *Ledger) Query(filter Filter) ([]Entry, error) {
	if l == nil {
		return nil, errors.New("prompt ledger disabled")
	}
	if filter.Until.IsZero() {
		filter.Until = l.now()
	}
	if filter.Since.IsZero() {
		filter.Since = filter.Until.Add(-24 * time.Hour)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultQueryLimit
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	names, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	firstDay := filter.Since.UTC().Truncate(24 * time.Hour)
	var out []Entry
	for _, name := range names {
		day, ok := dayOf(name.Name())
		if !ok || day.Before(firstDay) || day.After(filter.Until.UTC()) {
			continue
		}
		file, err := os.Open(filepath.Join(l.dir, name.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			var entry Entry
			if json.Unmarshal(scanner.Bytes(), &entry) != nil {
				continue
			}
			if entry.Time.Before(filter.Since) || entry.Time.After(filter.Until) || !filter.Matches(entry.Person) {
				continue
			}
			out = append(out, entry)
		}
		file.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// PersonFor names the person behind an identity: the tailnet login, else the
// node's tags for a tagged device, else Unattributed.
func PersonFor(identity Identity, ok bool) string {
	if !ok {
		return Unattributed
	}
	login := strings.TrimSpace(identity.LoginName)
	if login != "" && login != "tagged-devices" {
		return strings.ToLower(login)
	}
	if len(identity.Tags) > 0 {
		return strings.Join(identity.Tags, ",")
	}
	return Unattributed
}

// ExtractPrompt returns the human prompt of an Anthropic Messages request, a
// Codex/OpenAI Responses request, or a Codex WebSocket response.create event.
func ExtractPrompt(body []byte) (string, bool) {
	var probe struct {
		Type     string          `json:"type"`
		Messages json.RawMessage `json:"messages"`
		Input    json.RawMessage `json:"input"`
		Response struct {
			Input json.RawMessage `json:"input"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return "", false
	}
	if len(probe.Messages) > 0 {
		return ExtractClaudePrompt(body)
	}
	if probe.Type != "" && probe.Type != "response.create" {
		return "", false
	}
	input := probe.Input
	if len(input) == 0 {
		input = probe.Response.Input
	}
	return extractResponsesInput(input)
}

// extractResponsesInput reads the last input item. Only a user message with
// text that is not harness context (environment, AGENTS.md, instructions)
// counts; a function_call_output turn is the agent loop, not a person.
func extractResponsesInput(input json.RawMessage) (string, bool) {
	if len(input) == 0 {
		return "", false
	}
	var plain string
	if json.Unmarshal(input, &plain) == nil {
		plain = strings.TrimSpace(plain)
		return plain, plain != ""
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(input, &items) != nil || len(items) == 0 {
		return "", false
	}
	last := items[len(items)-1]
	if last.Role != "user" || (last.Type != "" && last.Type != "message") {
		return "", false
	}
	var text string
	if json.Unmarshal(last.Content, &text) == nil {
		text = strings.TrimSpace(text)
		if codexHarnessText(text) {
			return "", false
		}
		return text, text != ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(last.Content, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, block := range blocks {
		if block.Type != "input_text" && block.Type != "text" {
			continue
		}
		part := strings.TrimSpace(block.Text)
		if part == "" || codexHarnessText(part) {
			continue
		}
		parts = append(parts, part)
	}
	text = strings.Join(parts, "\n")
	return text, text != ""
}

// codexHarnessText is context Codex injects as a user message.
func codexHarnessText(text string) bool {
	for _, prefix := range []string{"<environment_context>", "# AGENTS.md instructions", "<user_instructions>", "<INSTRUCTIONS>", "<turn_aborted>", "<user_shell_command>"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// ExtractClaudePrompt returns the human text of the last user message in an
// Anthropic Messages request. A turn that only carries tool results (plus
// harness reminders) is not a human prompt and returns ok=false.
func ExtractClaudePrompt(body []byte) (string, bool) {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Messages) == 0 {
		return "", false
	}
	// Claude Code appends a "system" role message (the environment block)
	// after the user turn; skip trailing ones to reach the turn itself.
	index := len(request.Messages) - 1
	for index > 0 && request.Messages[index].Role == "system" {
		index--
	}
	last := request.Messages[index]
	if last.Role != "user" {
		return "", false
	}
	var plain string
	if json.Unmarshal(last.Content, &plain) == nil {
		plain = strings.TrimSpace(stripReminders(plain))
		return plain, plain != ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(last.Content, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "tool_result" {
			return "", false
		}
		if block.Type != "text" {
			continue
		}
		if text := strings.TrimSpace(stripReminders(block.Text)); text != "" {
			parts = append(parts, text)
		}
	}
	text := strings.Join(parts, "\n")
	return text, text != ""
}

// stripReminders removes <system-reminder>…</system-reminder> spans that the
// harness injects into user turns.
func stripReminders(text string) string {
	for {
		start := strings.Index(text, systemReminderTag)
		if start < 0 {
			return text
		}
		end := strings.Index(text[start:], "</system-reminder>")
		if end < 0 {
			return text[:start]
		}
		text = text[:start] + text[start+end+len("</system-reminder>"):]
	}
}
