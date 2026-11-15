package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// Token Plan accepts the subscription's Qwen 4 spelling but omits it from its
// catalog. Advertising both lets clients discover the common model without
// guessing aliases or losing selections saved under the subscription spelling.
func addQwenTokenCatalogAlias(response *http.Response) error {
	if response.StatusCode != http.StatusOK || response.Body == nil || response.Header.Get("Content-Encoding") != "" {
		return nil
	}
	const maxCatalogBytes = 1 << 20
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, maxCatalogBytes+1))
	response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
	if err != nil {
		return err
	}
	if len(prefix) > maxCatalogBytes {
		return nil
	}
	var catalog map[string]json.RawMessage
	if json.Unmarshal(prefix, &catalog) != nil {
		return nil
	}
	var models []map[string]json.RawMessage
	if json.Unmarshal(catalog["data"], &models) != nil {
		return nil
	}
	var source map[string]json.RawMessage
	for _, model := range models {
		var id string
		if json.Unmarshal(model["id"], &id) != nil {
			continue
		}
		if id == "qwen4.0-max-preview" {
			return nil
		}
		if id == "qwen4-max-preview" {
			source = model
		}
	}
	if source == nil {
		return nil
	}
	alias := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		alias[key] = value
	}
	alias["id"] = json.RawMessage(`"qwen4.0-max-preview"`)
	models = append(models, alias)
	catalog["data"], err = json.Marshal(models)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(encoded))
	response.ContentLength = int64(len(encoded))
	response.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	response.Header.Del("ETag")
	response.Header.Del("Content-MD5")
	response.Header.Del("Digest")
	return nil
}
