package mmmcp

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeOperationResultsForCurrentFrontendWire(t *testing.T) {
	meta := mcp.Meta{mcp.MetaKeyProtocolVersion: currentProtocolVersion}
	tests := []struct {
		name      string
		normalize func() (any, error)
		cacheable bool
	}{
		{
			name: "tools/call",
			normalize: func() (any, error) {
				request := &mcp.ServerRequest[*mcp.CallToolParams]{Params: &mcp.CallToolParams{Meta: meta}}
				return normalizeCallToolResult(request, &mcp.CallToolResult{Meta: downstreamServerMeta(), Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil)
			},
		},
		{
			name: "prompts/get",
			normalize: func() (any, error) {
				request := &mcp.ServerRequest[*mcp.GetPromptParams]{Params: &mcp.GetPromptParams{Meta: meta}}
				return normalizeGetPromptResult(request, &mcp.GetPromptResult{Meta: downstreamServerMeta(), Messages: []*mcp.PromptMessage{}})
			},
		},
		{
			name:      "resources/read",
			cacheable: true,
			normalize: func() (any, error) {
				request := &mcp.ServerRequest[*mcp.ReadResourceParams]{Params: &mcp.ReadResourceParams{Meta: meta}}
				return normalizeReadResourceResult(request, &mcp.ReadResourceResult{Meta: downstreamServerMeta(), Contents: []*mcp.ResourceContents{}})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.normalize()
			if err != nil {
				t.Fatal(err)
			}
			wire := marshalWireResult(t, result)
			if got := wireString(t, wire, "resultType"); got != "complete" {
				t.Fatalf("resultType = %q, want complete; wire = %s", got, mustJSON(t, result))
			}
			assertNoDownstreamServerInfo(t, wire)
			if test.cacheable {
				if got := wireString(t, wire, "cacheScope"); got != "public" {
					t.Fatalf("cacheScope = %q, want public", got)
				}
				if got := string(wire["ttlMs"]); got != "0" {
					t.Fatalf("ttlMs = %s, want 0", got)
				}
			}
		})
	}
}

func TestNormalizeOperationResultsForLegacyFrontendWire(t *testing.T) {
	tests := []struct {
		name      string
		normalize func() (any, error)
	}{
		{
			name: "tools/call",
			normalize: func() (any, error) {
				var downstream mcp.CallToolResult
				unmarshalCurrentResult(t, &downstream)
				return normalizeCallToolResult(&mcp.ServerRequest[*mcp.CallToolParams]{Params: &mcp.CallToolParams{}}, &downstream, nil)
			},
		},
		{
			name: "prompts/get",
			normalize: func() (any, error) {
				var downstream mcp.GetPromptResult
				unmarshalCurrentResult(t, &downstream)
				return normalizeGetPromptResult(&mcp.ServerRequest[*mcp.GetPromptParams]{Params: &mcp.GetPromptParams{}}, &downstream)
			},
		},
		{
			name: "resources/read",
			normalize: func() (any, error) {
				var downstream mcp.ReadResourceResult
				unmarshalCurrentResult(t, &downstream)
				return normalizeReadResourceResult(&mcp.ServerRequest[*mcp.ReadResourceParams]{Params: &mcp.ReadResourceParams{}}, &downstream)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.normalize()
			if err != nil {
				t.Fatal(err)
			}
			wire := marshalWireResult(t, result)
			if _, ok := wire["resultType"]; ok {
				t.Fatalf("legacy wire contains resultType: %s", mustJSON(t, result))
			}
			assertNoDownstreamServerInfo(t, wire)
		})
	}
}

func downstreamServerMeta() mcp.Meta {
	return mcp.Meta{mcp.MetaKeyServerInfo: map[string]any{"name": "downstream", "version": "1"}, "kept": true}
}

func unmarshalCurrentResult(t *testing.T, result any) {
	t.Helper()
	if err := json.Unmarshal([]byte(`{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"downstream","version":"1"},"kept":true},"resultType":"complete","content":[],"messages":[],"contents":[],"cacheScope":"public","ttlMs":0}`), result); err != nil {
		t.Fatal(err)
	}
}

func marshalWireResult(t *testing.T, result any) map[string]json.RawMessage {
	t.Helper()
	data := mustJSON(t, result)
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func wireString(t *testing.T, wire map[string]json.RawMessage, key string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(wire[key], &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertNoDownstreamServerInfo(t *testing.T, wire map[string]json.RawMessage) {
	t.Helper()
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(wire["_meta"], &meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := meta[mcp.MetaKeyServerInfo]; ok {
		t.Fatalf("downstream server info was forwarded: %s", wire["_meta"])
	}
	if _, ok := meta["kept"]; !ok {
		t.Fatalf("application metadata was discarded: %s", wire["_meta"])
	}
}
