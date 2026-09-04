package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

var antigravityProjectStore struct {
	sync.Mutex
	values map[string]string
}

// antigravityProjectFromBody extracts only the native Cloud Code envelope's
// top-level project. Nested request fields are intentionally ignored.
func antigravityProjectFromBody(body []byte) string {
	var envelope struct {
		Project string `json:"project"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &envelope) != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Project)
}

func (s *Server) antigravityProject(ctx context.Context, account accounts.Account, upstream *url.URL) (string, error) {
	if s == nil || upstream == nil || account.ID == "" || account.Token == "" {
		return "", errors.New("AGY project lookup is unavailable")
	}
	antigravityProjectStore.Lock()
	project := antigravityProjectStore.values[upstream.String()+"\x00"+account.ID]
	antigravityProjectStore.Unlock()
	if project != "" {
		return project, nil
	}
	endpoint := *upstream
	endpoint.Path = joinURLPath(upstream.Path, "/v1internal:loadCodeAssist")
	endpoint.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(`{"metadata":{"ideType":"ANTIGRAVITY","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+account.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: s.transport()}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("AGY project lookup failed: %s", res.Status)
	}
	var response struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "", errors.New("AGY project lookup returned malformed JSON")
	}
	project = antigravityProjectFromBody([]byte(`{"project":` + string(response.Project) + `}`))
	if project == "" {
		var object struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		}
		if json.Unmarshal(response.Project, &object) == nil {
			project = strings.TrimSpace(object.ID)
			if project == "" {
				project = strings.TrimSpace(object.ProjectID)
			}
		}
	}
	if project == "" {
		var text string
		_ = json.Unmarshal(response.Project, &text)
		project = strings.TrimSpace(text)
	}
	if project == "" {
		// Individual/free AGY accounts may legitimately return a successful
		// loadCodeAssist response without a companion project. The native CLI
		// can still provide its project in the generation envelope; preserve
		// that value rather than turning valid control-plane responses into a
		// relay 502. Callers rewrite only when a replacement project is known.
		return "", nil
	}
	antigravityProjectStore.Lock()
	if antigravityProjectStore.values == nil {
		antigravityProjectStore.values = make(map[string]string)
	}
	antigravityProjectStore.values[upstream.String()+"\x00"+account.ID] = project
	antigravityProjectStore.Unlock()
	return project, nil
}

func rewriteAntigravityProject(body []byte, project string) ([]byte, bool, error) {
	project = strings.TrimSpace(project)
	var envelope map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(body), &envelope) != nil || envelope == nil {
		return nil, false, errors.New("AGY request envelope is not JSON")
	}
	raw, ok := envelope["project"]
	if !ok {
		return body, false, nil
	}
	var existing string
	if json.Unmarshal(raw, &existing) != nil || strings.TrimSpace(existing) == "" {
		return nil, false, errors.New("AGY request envelope has an invalid project")
	}
	if project == "" || existing == project {
		return body, false, nil
	}
	value, _ := json.Marshal(project)
	envelope["project"] = value
	rebuilt, err := json.Marshal(envelope)
	return rebuilt, true, err
}
