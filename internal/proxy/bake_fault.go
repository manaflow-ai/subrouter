package proxy

import "net/http"

// injectedProxyFault, when set, makes the proxy answer a request with its own
// 502 instead of routing it. Release builds never set it: only a build with
// the subrouter_bakefault tag does (bake_fault_injection.go), which is how
// deploy/macos/tests/bakee2e produces a worker that starts, answers health,
// and still regresses routing, to prove the post-upgrade bake gate rolls it
// back.
var injectedProxyFault func(*http.Request) bool
