//go:build subrouter_bakefault

package proxy

import (
	"net/http"
	"sync/atomic"
)

// A deliberately broken worker for the bake gate end-to-end test: every third
// proxied request fails with a subrouter-generated 502. Never built without
// the subrouter_bakefault tag.
func init() {
	var requests atomic.Uint64
	injectedProxyFault = func(*http.Request) bool {
		return requests.Add(1)%3 == 0
	}
}
