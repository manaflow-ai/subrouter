package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/azureopenai"
)

func azureOpenAIProfileNameFromPath(requestPath string) (string, bool) {
	name, _, ok := stripAzureOpenAIPath(requestPath)
	return name, ok
}

func stripAzureOpenAIPath(requestPath string) (name, rest string, ok bool) {
	trimmed := strings.TrimPrefix(requestPath, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] != "azure" {
		return "", "", false
	}
	name, valid := azureopenai.ProfileNameFromAccountID(azureopenai.AccountID(parts[1]))
	if !valid || !strings.EqualFold(parts[1], name) {
		return "", "", false
	}
	if len(parts) == 2 {
		return name, "", true
	}
	return name, "/" + strings.Join(parts[2:], "/"), true
}

func azureOpenAIBasePath(requestPath string) bool {
	_, rest, ok := stripAzureOpenAIPath(requestPath)
	return ok && (rest == "" || rest == "/" || rest == "/v1" || rest == "/v1/")
}

func rewriteAzureOpenAIRequestModel(r *http.Request, requestedModel, deployment string, maxBodyBytes int64) error {
	if r == nil || requestedModel == deployment {
		return nil
	}
	if maxBodyBytes <= 0 {
		return errors.New("Azure OpenAI request body limit is not configured")
	}

	query := r.URL.Query()
	if values, exists := query["model"]; exists {
		for _, value := range values {
			if strings.TrimSpace(value) != requestedModel {
				return errors.New("Azure OpenAI request contains conflicting model selectors")
			}
		}
		query.Set("model", deployment)
		r.URL.RawQuery = query.Encode()
	}

	if r.Body == nil {
		return nil
	}
	if contentType := strings.ToLower(r.Header.Get("Content-Type")); !strings.Contains(contentType, "json") {
		return errors.New("Azure OpenAI model translation requires a JSON request body")
	}
	if encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); encoding != "" && encoding != "identity" {
		return fmt.Errorf("Azure OpenAI model translation does not support %s request encoding", encoding)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return errors.New("read Azure OpenAI request body")
	}
	if int64(len(body)) > maxBodyBytes {
		return errors.New("Azure OpenAI request body is too large")
	}
	if err := r.Body.Close(); err != nil {
		return errors.New("close Azure OpenAI request body")
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return errors.New("Azure OpenAI request body is not valid JSON")
	}
	rawModel, exists := payload["model"]
	if !exists {
		return errors.New("Azure OpenAI request body model is required")
	}
	var bodyModel string
	if err := json.Unmarshal(rawModel, &bodyModel); err != nil || strings.TrimSpace(bodyModel) != requestedModel {
		return errors.New("Azure OpenAI request contains conflicting model selectors")
	}
	payload["model"], _ = json.Marshal(deployment)
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return errors.New("encode Azure OpenAI request body")
	}
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(rewritten)), nil
	}
	r.ContentLength = int64(len(rewritten))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	return nil
}
