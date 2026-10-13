package mmmcp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/config"
	"github.com/obot-platform/mmmcp/testserver"
)

func TestPersistentFrontendSchemaProjection(t *testing.T) {
	fixture := testserver.New(t, testserver.Options{Tools: []testserver.Tool{{
		Definition: &mcp.Tool{
			Name:         "union",
			InputSchema:  map[string]any{"type": "object"},
			OutputSchema: map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"type": "object"}}},
		},
		Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: map[string]any{"ok": true}}, nil
		},
	}}})
	composite, err := New(t.Context(), &config.Config{Servers: []config.Server{{Name: "fixture", URL: fixture.URL}}}, Options{DSN: "sqlite://:memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = composite.Close() })
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			// Exercise RunStdio's persistent frontend path without process-global
			// stdin/stdout. HTTP and persistent sessions share one cached catalog.
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- composite.runPersistent(ctx, serverTransport) }()
			client := mcp.NewClient(&mcp.Implementation{Name: "persistent", Version: "test"}, nil)
			session, err := client.Connect(t.Context(), clientTransport, &mcp.ClientSessionOptions{ProtocolVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			listed, err := session.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(listed.Tools) != 1 {
				t.Fatalf("tools = %+v", listed.Tools)
			}
			data, err := json.Marshal(listed.Tools[0].OutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema map[string]any
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			if version == "2025-11-25" && schema["type"] != "object" {
				t.Fatalf("legacy persistent schema = %s", data)
			}
			if version == "2026-07-28" && schema["oneOf"] == nil {
				t.Fatalf("modern persistent schema was wrapped: %s", data)
			}
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "union"})
			if err != nil {
				t.Fatal(err)
			}
			var want any = map[string]any{"ok": true}
			if version == "2025-11-25" {
				want = map[string]any{"result": want}
			}
			if !reflect.DeepEqual(result.StructuredContent, want) {
				t.Fatalf("structured content = %#v, want %#v", result.StructuredContent, want)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("persistent frontend did not stop")
			}
		})
	}
}
