package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestWebSocketAdmissionBufferBudget(t *testing.T) {
	budget := newWebSocketByteBudget(8)
	first, err := readWebSocketQueuedMessage(context.Background(), websocket.TextMessage, strings.NewReader("first"), budget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readWebSocketQueuedMessage(context.Background(), websocket.TextMessage, strings.NewReader("next"), budget); !errors.Is(err, errWebSocketAdmissionBufferFull) {
		t.Fatalf("aggregate buffer overflow = %v", err)
	}
	first.release()
	first.release()
	if budget.used != 0 {
		t.Fatalf("buffer reservation leaked: %d", budget.used)
	}
	broken := io.MultiReader(strings.NewReader("part"), errorWebSocketReader{err: io.ErrUnexpectedEOF})
	if _, err := readWebSocketQueuedMessage(context.Background(), websocket.TextMessage, broken, budget); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial read = %v", err)
	}
	if budget.used != 0 {
		t.Fatalf("partial read retained %d bytes", budget.used)
	}
	// A queued input cannot consume the separate response budget needed to
	// receive the terminal event that releases its admission slot.
	for _, veto := range []bool{false, true} {
		limit := int64(4)
		wantErr := errWebSocketAdmissionBufferFull
		if veto {
			limit = 8
			wantErr = errors.New("message vetoed")
		}
		responseBudget := newWebSocketByteBudget(limit)
		writer := &bufferedWebSocketWriter{budget: responseBudget}
		observer := newWebSocketMessageObserver(nil, "codex", "session", "upstream_to_client", websocket.TextMessage)
		observer.chunked = true // No retained transcript body is available.
		_, release, err := streamWebSocketMessage(context.Background(), strings.NewReader("reply"), func() (io.WriteCloser, error) {
			return writer, nil
		}, observer, newWebSocketCopyBufferPool(newWebSocketByteBudget(8), 4), func(body []byte) error {
			if string(body) != "reply" {
				t.Fatalf("terminal event body lost when transcript capture was unavailable: %q", body)
			}
			return wantErr
		})
		if release != nil {
			release()
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("response buffer overflow = %v (veto=%v)", err, veto)
		}
		if responseBudget.used != 0 {
			t.Fatalf("response overflow retained %d bytes", responseBudget.used)
		}
	}
}

func TestWebSocketAdmissionCountsTurns(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.failed", "response.incomplete", "response.done", "error", "disconnect", "queued_client_disconnect", "queued_upstream_disconnect"} {
		t.Run(terminal, func(t *testing.T) {
			gate := newAdmissionControl()
			gate.limit = 1
			ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			type turn struct {
				conn *websocket.Conn
				body string
			}
			started := make(chan turn, 8)
			connected := make(chan *websocket.Conn, 8)
			closed := make(chan error, 8)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				connected <- conn
				for {
					_, body, err := conn.ReadMessage()
					if err != nil {
						closed <- err
						return
					}
					started <- turn{conn, string(body)}
				}
			}))
			t.Cleanup(upstream.Close)
			upstreamURL, _ := url.Parse(upstream.URL)
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(Server{
				Upstream: upstreamURL,
				Accounts: []accounts.Account{{ID: "account", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token"}},
				Sessions: store, SchedulerRef: ref, admission: gate, MaxBodyBytes: 1 << 20,
			}.Handler())
			t.Cleanup(server.Close)
			dial := websocket.Dialer{HandshakeTimeout: time.Second}
			connect := func() *websocket.Conn {
				t.Helper()
				conn, response, err := dial.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{
					"X-Subrouter-Account-ID": {"account"}, "X-Subrouter-Agent": {"codex"}, "X-Subrouter-Session": {"same-reviewer"},
				})
				if response != nil && response.Body != nil {
					response.Body.Close()
				}
				if err != nil {
					t.Fatalf("idle/reviewer connection blocked: %v", err)
				}
				t.Cleanup(func() { conn.Close() })
				return conn
			}
			first, second := connect(), connect()
			<-connected
			secondUpstream := <-connected
			if counts := ref.InflightCounts(); len(counts) != 0 {
				t.Fatalf("idle sockets counted as inference: %v", counts)
			}
			create := `{"type":"response.create","model":"gpt-6.1-sol","input":[]}`
			send := func(conn *websocket.Conn) {
				t.Helper()
				if err := conn.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
					t.Fatal(err)
				}
			}
			receive := func() turn {
				t.Helper()
				select {
				case got := <-started:
					if got.body != create {
						t.Fatalf("upstream payload = %q", got.body)
					}
					return got
				case <-time.After(time.Second):
					t.Fatal("admitted turn did not reach upstream")
					return turn{}
				}
			}
			send(first)
			active := receive()
			if counts := ref.InflightCounts(); counts[selectacct.ScoreKey(accounts.ProviderCodex, "account")] != 1 {
				t.Fatalf("active turn inflight = %v", counts)
			}
			send(second)
			waitWebSocketAdmission(t, gate, 1, 1)
			select {
			case got := <-started:
				t.Fatalf("queued turn reached upstream before admission: %q", got.body)
			default:
			}
			if strings.HasPrefix(terminal, "queued_") {
				// A pipelined create must not hide the following disconnect
				// from the reader while the first create waits for admission.
				send(second)
				if terminal == "queued_client_disconnect" {
					if err := second.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "leaving"), time.Now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					second.Close()
				} else {
					secondUpstream.Close()
				}
				waitWebSocketAdmission(t, gate, 1, 0)
				if terminal == "queued_client_disconnect" {
					select {
					case err := <-closed:
						var closeErr *websocket.CloseError
						if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseGoingAway || closeErr.Text != "leaving" {
							t.Fatalf("client close code/reason changed: %v", err)
						}
					case <-time.After(time.Second):
						t.Fatal("client close did not reach upstream")
					}
				}
				active.conn.Close()
				waitWebSocketAdmission(t, gate, 0, 0)
				return
			}
			if terminal == "disconnect" {
				active.conn.Close()
			} else {
				if err := active.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":%q}`, terminal))); err != nil {
					t.Fatal(err)
				}
				first.SetReadDeadline(time.Now().Add(time.Second))
				if _, _, err := first.ReadMessage(); err != nil {
					t.Fatal(err)
				}
			}
			next := receive()
			if err := next.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)); err != nil {
				t.Fatal(err)
			}
			second.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := second.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			waitWebSocketAdmission(t, gate, 0, 0)
			if counts := ref.InflightCounts(); len(counts) != 0 {
				t.Fatalf("finished turns retained inflight counts: %v", counts)
			}
		})
	}
}

func waitWebSocketAdmission(t *testing.T, gate *admissionControl, active, queued int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		snapshot := gate.snapshot()
		gotActive, gotQueued := 0, 0
		for _, bucket := range snapshot.Accounts {
			gotActive += bucket.Active
			gotQueued += bucket.Queued
		}
		if gotActive == active && gotQueued == queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission active/queued = %d/%d, want %d/%d", gotActive, gotQueued, active, queued)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWebSocketAdmissionRejectsClientWithRetryHint(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprint("paused=", paused), func(t *testing.T) {
			gate := newAdmissionControl()
			gate.limit, gate.queueCap = 1, 0
			key := admissionKeyFor(accounts.ProviderCodex, "account", "gpt-6.1-sol")
			want := errAdmissionQueueFull.Error()
			if paused {
				gate.pause(key, time.Now().Add(time.Hour))
				want = errAdmissionPaused.Error()
			} else {
				lease, err := gate.acquire(context.Background(), key)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.release()
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err == nil {
					t.Error("admission-rejected turn reached upstream")
				}
			}))
			t.Cleanup(upstream.Close)
			upstreamURL, _ := url.Parse(upstream.URL)
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(Server{
				Upstream: upstreamURL, admission: gate, Sessions: store,
				Accounts:  []accounts.Account{{ID: "account", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token"}},
				Scheduler: selectacct.NewScheduler(nil), MaxBodyBytes: 1 << 20,
			}.Handler())
			t.Cleanup(proxy.Close)
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			conn.SetReadDeadline(time.Now().Add(time.Second))
			_, _, err = conn.ReadMessage()
			var closeErr *websocket.CloseError
			if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater || !strings.Contains(closeErr.Text, want) || !strings.Contains(closeErr.Text, "retry after") {
				t.Fatalf("client close = %v, want1013 with admission reason and retry hint", err)
			}
		})
	}
}

func TestBrokeredWebSocketDoesNotUseLocalPoolAdmission(t *testing.T) {
	gate := newAdmissionControl()
	gate.pause(admissionKeyFor(accounts.ProviderCodex, "leased", "gpt-6.1-sol"), time.Now().Add(time.Hour))
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
		conn.ReadMessage()
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	leased := &fakeCredentialBroker{
		lease:   broker.Lease{ID: "lease", Account: accounts.Account{ID: "leased", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token"}, ExpiresAt: time.Now().Add(time.Minute)},
		reports: make(chan broker.LeaseOutcome, 2),
	}
	proxy := httptest.NewServer(Server{Upstream: upstreamURL, CredentialBroker: leased, admission: gate, SchedulerRef: ref, MaxBodyBytes: 1 << 20}.Handler())
	t.Cleanup(proxy.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":[]}`))
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, body, err := conn.ReadMessage(); err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("local admission overrode broker lease: %s, %v", body, err)
	}
	if len(ref.InflightCounts()) != 0 {
		t.Fatal("broker-owned request entered the local scheduler's inflight counts")
	}
	for _, bucket := range gate.snapshot().Accounts {
		if bucket.Active != 0 || bucket.Served != 0 {
			t.Fatalf("broker-owned request entered local pool admission: %+v", bucket)
		}
	}
}
