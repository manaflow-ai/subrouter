package claude

import "github.com/manaflow-ai/subrouter/internal/credshape"

// unreadableCredentialPhrase appears in every credential-decode error. A
// credential that will not parse cannot be refreshed without re-auth, so the
// proxy classifies this phrase as a terminal credential error rather than a
// transient one.
const unreadableCredentialPhrase = "unreadable credential"

// describeCredentialPayload summarizes a payload that failed to decode without
// revealing any of it. The shared describer is used by every credential store
// that has to diagnose a malformed blob.
func describeCredentialPayload(body []byte, err error) string {
	return credshape.Describe(body, err)
}
