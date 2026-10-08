package mmmcp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/obot-platform/mmmcp"
	"github.com/obot-platform/mmmcp/config"
)

func TestCurrentProtocolListResultsHaveAppropriateCacheScopeAndAreImmediatelyStale(t *testing.T) {
	fixture := namedToolFixture(t, "ok")
	composite, err := mmmcp.New(t.Context(), &config.Config{
		Servers: []config.Server{{Name: "fixture", URL: fixture.URL}},
	}, mmmcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer composite.Close()
	frontend := httptest.NewServer(composite.HTTPHandler())
	defer frontend.Close()

	for i, method := range []string{"tools/list", "prompts/list", "resources/list", "resources/templates/list"} {
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      i + 1,
			"method":  method,
			"params": map[string]any{
				"_meta": map[string]any{
					"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
					"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, frontend.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", method)
		response, err := frontend.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", method, response.StatusCode, responseBody)
		}
		var message struct {
			Result struct {
				CacheScope string `json:"cacheScope"`
				TTLMs      *int   `json:"ttlMs"`
			} `json:"result"`
		}
		if err := json.Unmarshal(responseBody, &message); err != nil {
			t.Fatalf("%s response decode: %v; body = %s", method, err, responseBody)
		}
		wantScope := "public"
		if method == "tools/list" {
			wantScope = "private"
		}
		if message.Result.CacheScope != wantScope || message.Result.TTLMs == nil || *message.Result.TTLMs != 0 {
			t.Fatalf("%s caching = scope %q, ttl %v; body = %s", method, message.Result.CacheScope, message.Result.TTLMs, responseBody)
		}
	}
}
