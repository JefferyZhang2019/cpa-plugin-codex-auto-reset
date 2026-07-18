package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func makeUnsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return strings.Join([]string{header, payload, ""}, ".")
}

func TestExtractCodexCredentialsTopLevel(t *testing.T) {
	idToken := makeUnsignedJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_123",
		},
	})
	raw := json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`)
	creds, err := ExtractCodexCredentials(raw)
	if err != nil {
		t.Fatalf("ExtractCodexCredentials returned error: %v", err)
	}
	if creds.AccessToken != "access-1" || creds.ChatGPTAccountID != "acct_123" {
		t.Fatalf("creds = %#v", creds)
	}
}

func TestExtractCodexCredentialsNestedMetadata(t *testing.T) {
	idToken := makeUnsignedJWT(t, map[string]any{
		"https://api.openai.com/auth.chatgpt_account_id": "acct_nested",
	})
	raw := json.RawMessage(`{"tokens":{"access_token":"access-2"},"metadata":{"id_token":"` + idToken + `"}}`)
	creds, err := ExtractCodexCredentials(raw)
	if err != nil {
		t.Fatalf("ExtractCodexCredentials returned error: %v", err)
	}
	if creds.AccessToken != "access-2" || creds.ChatGPTAccountID != "acct_nested" {
		t.Fatalf("creds = %#v", creds)
	}
}

func TestExtractCodexCredentialsUsesStoredAccountIDWhenIDTokenMissing(t *testing.T) {
	raw := json.RawMessage(`{"access_token":"access-3","refresh_token":"refresh-3","account_id":"acct_stored","expired":"2026-06-21T10:00:00Z"}`)
	creds, err := ExtractCodexCredentials(raw)
	if err != nil {
		t.Fatalf("ExtractCodexCredentials returned error: %v", err)
	}
	if creds.AccessToken != "access-3" || creds.RefreshToken != "refresh-3" || creds.ChatGPTAccountID != "acct_stored" || creds.ExpiresAt.IsZero() {
		t.Fatalf("creds = %#v", creds)
	}
}

func TestExtractCodexCredentialsRejectsMissingAccessToken(t *testing.T) {
	_, err := ExtractCodexCredentials(json.RawMessage(`{"id_token":"x.y.z"}`))
	if err == nil {
		t.Fatalf("ExtractCodexCredentials accepted missing access token")
	}
}
