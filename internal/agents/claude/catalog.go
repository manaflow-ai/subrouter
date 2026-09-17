package claude

import "strings"

// ModelSpec is the single Subrouter-owned description of a Claude model that
// can be selected by a pooled client. Model is the wire ID sent to the proxy;
// BehavesAs tells Claude Code which built-in family semantics to apply when a
// client release does not know that ID yet.
type ModelSpec struct {
	Model       string `json:"model"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	BehavesAs   string `json:"behavesAs,omitempty"`
	Family      string `json:"-"`
}

// ModelCatalog is the catalog projected into Claude Code's model picker. Keep
// this list in the agent package so the picker and quota routing share the same
// model-family vocabulary.
func ModelCatalog() []ModelSpec {
	return []ModelSpec{
		{Model: FableModel, Label: "Fable", Description: "Pooled Fable", BehavesAs: "fable", Family: FableFeature},
		{Model: "claude-opus-5", Label: "Opus", Description: "Pooled Opus", BehavesAs: "opus", Family: OpusFeature},
		{Model: "claude-sonnet-5", Label: "Sonnet", Description: "Pooled Sonnet", BehavesAs: "sonnet", Family: SonnetFeature},
		{Model: "claude-haiku-4-5", Label: "Haiku", Description: "Pooled Haiku"},
	}
}

// ModelFamily returns the quota family for a Claude model ID. Versioned IDs
// and aliases remain compatible with the catalog, while unknown providers do
// not get accidentally assigned to a Claude pool.
func ModelFamily(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, spec := range ModelCatalog() {
		if spec.Family != "" && (lower == strings.ToLower(spec.Model) || strings.Contains(lower, strings.TrimPrefix(spec.Family, "claude-")) || strings.EqualFold(lower, spec.BehavesAs)) {
			return spec.Family
		}
	}
	return ""
}
