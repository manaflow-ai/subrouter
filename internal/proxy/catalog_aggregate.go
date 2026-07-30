package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Codex crawls the plugin catalog to exhaustion and its memory cost grows with
// the number of pages it walks, not the number of entries it receives: one page
// of 1,000 entries costs ~125 MB, while two pages of 200 cost ~9.6 GB and the
// full 14-page crawl reaches 20 GB and can panic a machine. Codex only avoids
// this against chatgpt.com because its catalog disk cache is keyed on
// chatgpt_base_url, so a warm cache skips the crawl; any proxy is a cold key by
// definition and cannot proxy its way out of it.
//
// So subrouter walks the pagination itself and hands back one page. The client
// gets the whole catalog, sees no continuation token, and never paginates.
const (
	// catalogAggregateMaxPages bounds subrouter's own walk.
	catalogAggregateMaxPages = 40
	// catalogAggregateMaxBytes bounds it by size as well as page count.
	catalogAggregateMaxBytes = 128 << 20
)

func isCatalogListPath(path string) bool {
	p := path
	if stripped, ok := stripChatGPTBackendPath(path); ok {
		p = stripped
	}
	return strings.HasPrefix(p, "/ps/plugins/list")
}

func catalogNextPageToken(body []byte) string {
	var decoded struct {
		Pagination struct {
			NextPageToken string `json:"next_page_token"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ""
	}
	return decoded.Pagination.NextPageToken
}

func catalogPlugins(body []byte) ([]json.RawMessage, bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	raw, ok := payload["plugins"]
	if !ok {
		return nil, false
	}
	var plugins []json.RawMessage
	if err := json.Unmarshal(raw, &plugins); err != nil {
		return nil, false
	}
	return plugins, true
}

// aggregateCatalogPages follows the catalog cursor upstream and returns a
// single page carrying every entry, with no continuation token. It returns
// (body, pages, entries, true) only when it actually merged something.
func aggregateCatalogPages(
	rt http.RoundTripper,
	template *http.Request,
	upstream *url.URL,
	firstBody []byte,
) ([]byte, int, int, bool) {
	if rt == nil || template == nil || upstream == nil {
		return firstBody, 0, 0, false
	}
	if !isCatalogListPath(template.URL.Path) {
		return firstBody, 0, 0, false
	}
	token := catalogNextPageToken(firstBody)
	if token == "" {
		return firstBody, 0, 0, false
	}
	merged, ok := catalogPlugins(firstBody)
	if !ok {
		return firstBody, 0, 0, false
	}

	pages := 1
	bytes := len(firstBody)
	for token != "" && pages < catalogAggregateMaxPages && bytes < catalogAggregateMaxBytes {
		next := template.Clone(template.Context())
		next.RequestURI = ""
		nextURL := *template.URL
		nextURL.Scheme = upstream.Scheme
		nextURL.Host = upstream.Host
		// The reverse proxy joins the upstream's own path prefix onto the
		// request path; re-issuing by hand has to do the same or it 404s.
		if prefix := strings.TrimSuffix(upstream.Path, "/"); prefix != "" &&
			!strings.HasPrefix(nextURL.Path, prefix+"/") {
			nextURL.Path = prefix + nextURL.Path
		}
		q := nextURL.Query()
		q.Set("pageToken", token)
		nextURL.RawQuery = q.Encode()
		next.URL = &nextURL
		next.Host = upstream.Host
		next.Body = nil
		next.GetBody = nil
		next.ContentLength = 0

		resp, err := rt.RoundTrip(next)
		if err != nil {
			break
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, catalogAggregateMaxBytes))
		resp.Body.Close()
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			break
		}
		plugins, ok := catalogPlugins(body)
		if !ok {
			break
		}
		merged = append(merged, plugins...)
		bytes += len(body)
		pages++
		token = catalogNextPageToken(body)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(firstBody, &payload); err != nil {
		return firstBody, 0, 0, false
	}
	mergedPlugins, err := json.Marshal(merged)
	if err != nil {
		return firstBody, 0, 0, false
	}
	payload["plugins"] = mergedPlugins
	if rawPagination, ok := payload["pagination"]; ok {
		var pagination map[string]json.RawMessage
		if err := json.Unmarshal(rawPagination, &pagination); err == nil {
			delete(pagination, "next_page_token")
			if patched, err := json.Marshal(pagination); err == nil {
				payload["pagination"] = patched
			}
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return firstBody, 0, 0, false
	}
	return out, pages, len(merged), true
}
