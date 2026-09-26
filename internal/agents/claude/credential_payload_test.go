package claude

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const validCredentialPayload = `{"claudeAiOauth":{"accessToken":"sk-ant-oat-secret","refreshToken":"sk-ant-ort-secret","expiresAt":1799999999999}}`

func TestParseCredentialPayloadDecodesValidBlob(t *testing.T) {
	credential, err := parseCredentialPayload([]byte(validCredentialPayload), "keychain")
	if err != nil {
		t.Fatalf("valid payload must decode: %v", err)
	}
	if credential == nil || credential.AccessToken != "sk-ant-oat-secret" {
		t.Fatalf("decoded credential did not round-trip: %+v", credential)
	}
}

func TestParseCredentialPayloadDecodesMacOSKeychainHexData(t *testing.T) {
	body := make([]byte, hex.EncodedLen(len(validCredentialPayload)))
	hex.Encode(body, []byte(validCredentialPayload))
	body = append(body, '\n')
	credential, err := parseCredentialPayload(body, "keychain")
	if err != nil {
		t.Fatalf("hex keychain payload must decode: %v", err)
	}
	if credential == nil || credential.AccessToken != "sk-ant-oat-secret" {
		t.Fatalf("decoded credential did not round-trip: %+v", credential)
	}
}

func TestParseCredentialPayloadRejectsHexOutsideKeychain(t *testing.T) {
	body := make([]byte, hex.EncodedLen(len(validCredentialPayload)))
	hex.Encode(body, []byte(validCredentialPayload))
	if _, err := parseCredentialPayload(body, "credentials file"); err == nil {
		t.Fatal("hex text in a credential file must not be treated as keychain data")
	}
}

func TestParseCredentialPayloadRejectsHexThatIsNotCredentialJSON(t *testing.T) {
	body := make([]byte, hex.EncodedLen(len("not json")))
	hex.Encode(body, []byte("not json"))
	if _, err := parseCredentialPayload(body, "keychain"); err == nil {
		t.Fatal("hex keychain data must decode to a complete credential payload")
	}
}

// The production symptom was `invalid character 'b' after top-level value`,
// repeated thousands of times with no indication of what the payload actually
// was. The decode error must name the source and the shape.
func TestParseCredentialPayloadReportsSourceAndShape(t *testing.T) {
	body := []byte(validCredentialPayload + "bplist00\x00\x01")
	_, err := parseCredentialPayload(body, "keychain")
	if err == nil {
		t.Fatal("a payload with trailing bytes must not decode")
	}
	message := err.Error()
	for _, want := range []string{
		unreadableCredentialPhrase,
		"from keychain",
		"trailing_kind=binary-plist",
		"invalid character 'b' after top-level value",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("decode error %q is missing %q", message, want)
		}
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatal("the underlying json error must stay unwrappable")
	}
}

// The payload holds an access token, and trailing bytes can be the tail of a
// previous token, so no part of it may reach a log line.
func TestParseCredentialPayloadNeverEchoesTheBlob(t *testing.T) {
	secrets := []string{"sk-ant-oat-secret", "sk-ant-ort-secret"}
	bodies := [][]byte{
		[]byte(validCredentialPayload + `xToken":"sk-ant-ort-leftover"}}`),
		[]byte(validCredentialPayload + "bplist00"),
		[]byte(validCredentialPayload + "\x00\x00\x00"),
		[]byte(validCredentialPayload + " trailing text"),
		append([]byte(validCredentialPayload), 0xff, 0xfe),
	}
	for _, body := range bodies {
		_, err := parseCredentialPayload(body, "keychain")
		if err == nil {
			t.Fatalf("payload %q should not decode", trimForName(body))
		}
		message := err.Error()
		for _, secret := range secrets {
			if strings.Contains(message, secret) {
				t.Fatalf("decode error leaked a secret: %q", message)
			}
		}
		if strings.Contains(message, "leftover") {
			t.Fatalf("decode error leaked trailing payload bytes: %q", message)
		}
	}
}

// A non-syntax decode failure (well-formed JSON, wrong shape) still reports the
// size but has no meaningful trailing region.
func TestDescribeCredentialPayloadWithoutSyntaxError(t *testing.T) {
	body := []byte(`{"claudeAiOauth":"not-an-object"}`)
	_, err := parseCredentialPayload(body, "credentials file")
	if err == nil {
		t.Fatal("a type mismatch must not decode")
	}
	if !strings.Contains(err.Error(), "trailing=unknown") {
		t.Fatalf("expected an unknown trailing region, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "from credentials file") {
		t.Fatalf("expected the file source to be named, got %q", err.Error())
	}
}

func trimForName(body []byte) string {
	if len(body) > 32 {
		return string(body[:32]) + "..."
	}
	return string(body)
}
