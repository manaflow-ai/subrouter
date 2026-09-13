package proxy

import (
	"net/http"
	"testing"
)

func TestProxyMethodAllowedForForwardCompatibility(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		if !proxyMethodAllowed(method) {
			t.Fatalf("%s must remain forwardable", method)
		}
	}
	for _, method := range []string{http.MethodConnect, http.MethodTrace} {
		if proxyMethodAllowed(method) {
			t.Fatalf("%s must not be forwardable", method)
		}
	}
}
