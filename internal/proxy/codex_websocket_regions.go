package proxy

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"
)

// Codex websocket regions.
//
// A websocket turn cannot be moved mid-stream, but its dial can go out of
// any region (codex_regions.go): an egress proxy is an HTTP CONNECT tunnel,
// which carries a websocket upgrade as well as an HTTPS request. So:
//
//   - a session pinned to an egress dials through it, where it used to be
//     refused with 426 and pushed onto the HTTP transport;
//   - a new session avoids the local uplink while it is cooling for the
//     model, and skips a proxy that cannot be reached;
//   - an autonomous turn that hits capacity redials in the next healthy
//     region instead of resending on the same socket, unless it chains on a
//     previous_response_id that only the old connection holds.

type codexWebSocketRegionPlanKey struct{}

// codexWebSocketRegionPlan is the ordered list of slots a websocket dial
// tries, carried from the routing handler to proxyWebSocket.
type codexWebSocketRegionPlan struct {
	slots []int
	model string
}

func withCodexWebSocketRegionPlan(ctx context.Context, plan *codexWebSocketRegionPlan) context.Context {
	return context.WithValue(ctx, codexWebSocketRegionPlanKey{}, plan)
}

func codexWebSocketRegionPlanFrom(ctx context.Context) *codexWebSocketRegionPlan {
	plan, _ := ctx.Value(codexWebSocketRegionPlanKey{}).(*codexWebSocketRegionPlan)
	return plan
}

// planCodexWebSocketRegions orders the dial: the session's pinned egress
// first, then healthy regions (local leading), then cooling ones. Faulted
// regions are left out.
func (s Server) planCodexWebSocketRegions(ctx context.Context, agentType, sessionID, model string) *codexWebSocketRegionPlan {
	health := s.codexRegions
	if health.slots() <= 1 {
		return nil
	}
	key := azureCodexSessionKeyFor(agentType, sessionID)
	keep := -1
	if pinned, ok := s.codexEgressSessions.lookup(key); ok && pinned >= 0 && pinned < health.slots()-1 {
		keep = pinned + 1
	}
	fault := codexFaultFrom(ctx)
	start := azureCodexEndpointIndex(key, health.slots()-1)
	slots := health.order(model, health.slots()-1, start, health.slots(), keep, func(slot int) bool {
		return fault.blocks(health.name(slot))
	})
	if len(slots) == 0 {
		return nil
	}
	return &codexWebSocketRegionPlan{slots: slots, model: model}
}

// codexEgressWebSocketDialers mirrors outboundWebSocketDialer (http/1.1,
// IPv4, handshake timeout) with each egress as the CONNECT proxy.
func codexEgressWebSocketDialers(config *CodexEgressConfig) []*websocket.Dialer {
	if !config.configured() {
		return nil
	}
	base := outboundWebSocketDialer()
	dialers := make([]*websocket.Dialer, 0, len(config.Proxies))
	for _, proxy := range config.Proxies {
		dialer := *base
		dialer.Proxy = http.ProxyURL(proxy)
		if base.TLSClientConfig != nil {
			dialer.TLSClientConfig = base.TLSClientConfig.Clone()
		}
		dialers = append(dialers, &dialer)
	}
	return dialers
}

func (s Server) codexWebSocketDialer(slot int) *websocket.Dialer {
	if slot > codexRegionLocalSlot && slot-1 < len(s.codexEgressWebSocketDialers) {
		return s.codexEgressWebSocketDialers[slot-1]
	}
	return outboundWebSocketDialer()
}

// dialCodexWebSocket dials the first region of plan that answers. A region
// that cannot even be reached (no upstream response) is noted and the next
// one is tried; an upstream refusal ends the loop, it is the account's.
func (s Server) dialCodexWebSocket(ctx context.Context, plan *codexWebSocketRegionPlan, target string, headers http.Header) (*websocket.Conn, *http.Response, int, error) {
	slots := []int{codexRegionLocalSlot}
	model := ""
	if plan != nil && len(plan.slots) > 0 {
		slots, model = plan.slots, plan.model
	}
	var conn *websocket.Conn
	var response *http.Response
	var err error
	slot := slots[0]
	for index, candidate := range slots {
		slot = candidate
		conn, response, err = s.codexWebSocketDialer(slot).DialContext(ctx, target, headers)
		if err == nil || response != nil || ctx.Err() != nil || index == len(slots)-1 {
			break
		}
		if slot != codexRegionLocalSlot {
			s.codexRegions.note(model, slot, true)
		}
		if s.Logger != nil {
			s.Logger.Warn("codex websocket region unreachable; dialing the next one",
				"region", s.codexRegions.name(slot), "error", err)
		}
	}
	return conn, response, slot, err
}

// Region state on the link.

func (l *webSocketUpstreamLink) setRegion(slot int) {
	l.mu.Lock()
	l.region = slot
	if l.regionTried == nil {
		l.regionTried = map[int]struct{}{}
	}
	l.regionTried[slot] = struct{}{}
	l.mu.Unlock()
}

func (l *webSocketUpstreamLink) currentRegion() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.region
}

// requestRotate makes the next redial leave the current region.
func (l *webSocketUpstreamLink) requestRotate() {
	l.mu.Lock()
	l.rotate = true
	l.mu.Unlock()
}

// nextRedialSlot is the region for the next redial: the current one, or
// after a capacity failure the next untried region, healthy first.
func (l *webSocketUpstreamLink) nextRedialSlot(health *codexRegionHealth, model string, fault *codexFault, sessionKey string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.rotate || health.slots() <= 1 {
		return l.region
	}
	l.rotate = false
	if l.regionTried == nil {
		l.regionTried = map[int]struct{}{}
	}
	start := azureCodexEndpointIndex(sessionKey, health.slots()-1)
	pick := func() []int {
		return health.order(model, health.slots()-1, start, 1, -1, func(slot int) bool {
			_, done := l.regionTried[slot]
			return done || fault.blocks(health.name(slot))
		})
	}
	order := pick()
	if len(order) == 0 {
		clear(l.regionTried)
		l.regionTried[l.region] = struct{}{}
		order = pick()
	}
	if len(order) == 0 {
		return l.region
	}
	return order[0]
}

func (l *webSocketUpstreamLink) noteTurnCapacityFailure() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turnCapacityFailures++
	return l.turnCapacityFailures
}

// takeTurnCapacityFailures returns and resets the finished turn's count.
func (l *webSocketUpstreamLink) takeTurnCapacityFailures() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.turnCapacityFailures
	l.turnCapacityFailures = 0
	return n
}
