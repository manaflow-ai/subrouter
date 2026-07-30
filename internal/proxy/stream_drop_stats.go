package proxy

import (
	"sync/atomic"
	"time"
)

// StreamDropStats counts response-stream terminations by which side ended them.
//
// Storage is the constraint this type exists to solve. A dropped stream happens
// roughly 1000 times a day at current volume, and ~94% of those are the client
// hanging up and immediately retrying, which is expected behavior rather than a
// defect. Writing an ERROR line for each one buys nothing and is a large share
// of an unrotated log. Counting them costs a few atomic adds and stays flat
// forever, while the rare canceled_by=proxy case still gets a full line each.
type StreamDropStats struct {
	client   atomic.Uint64
	proxy    atomic.Uint64
	upstream atomic.Uint64
	unknown  atomic.Uint64

	lastProxyUnix atomic.Int64
	sinceUnix     atomic.Int64
}

// StreamDropSnapshot is a point-in-time read of the counters.
type StreamDropSnapshot struct {
	Client    uint64 `json:"client"`
	Proxy     uint64 `json:"proxy"`
	Upstream  uint64 `json:"upstream"`
	Unknown   uint64 `json:"unknown"`
	Total     uint64 `json:"total"`
	Since     string `json:"since,omitempty"`
	LastProxy string `json:"last_proxy_drop,omitempty"`
}

// Observe records one dropped stream. Safe on a nil receiver so the counters
// stay optional for callers that do not wire them up.
func (s *StreamDropStats) Observe(canceledBy string, now time.Time) {
	if s == nil {
		return
	}
	s.sinceUnix.CompareAndSwap(0, now.Unix())
	switch canceledBy {
	case "client":
		s.client.Add(1)
	case "proxy":
		s.proxy.Add(1)
		s.lastProxyUnix.Store(now.Unix())
	case "upstream":
		s.upstream.Add(1)
	default:
		s.unknown.Add(1)
	}
}

// Snapshot reads the counters without resetting them.
func (s *StreamDropStats) Snapshot() StreamDropSnapshot {
	if s == nil {
		return StreamDropSnapshot{}
	}
	snapshot := StreamDropSnapshot{
		Client:   s.client.Load(),
		Proxy:    s.proxy.Load(),
		Upstream: s.upstream.Load(),
		Unknown:  s.unknown.Load(),
	}
	snapshot.Total = snapshot.Client + snapshot.Proxy + snapshot.Upstream + snapshot.Unknown
	if since := s.sinceUnix.Load(); since != 0 {
		snapshot.Since = time.Unix(since, 0).UTC().Format(time.RFC3339)
	}
	if last := s.lastProxyUnix.Load(); last != 0 {
		snapshot.LastProxy = time.Unix(last, 0).UTC().Format(time.RFC3339)
	}
	return snapshot
}
