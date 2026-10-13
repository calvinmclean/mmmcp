package mmmcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWrapOutputSchemaForLegacy(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   string
	}{
		{
			name:   "local pointers and anchors",
			schema: `{"anyOf":[{"$ref":"#"},{"$ref":"#/$defs/item"},{"$dynamicRef":"#/items"},{"$ref":"#named"},{"$ref":"other.json#/items"}],"$anchor":"named","$dynamicAnchor":"dynamic"}`,
			want:   `{"anyOf":[{"$ref":"#/properties/result"},{"$ref":"#/properties/result/$defs/item"},{"$dynamicRef":"#/properties/result/items"},{"$ref":"#named"},{"$ref":"other.json#/items"}],"$anchor":"named","$dynamicAnchor":"dynamic"}`,
		},
		{
			name:   "literal data",
			schema: `{"const":{"$ref":"#"},"enum":[{"$dynamicRef":"#/items"}],"default":{"$id":"other","$ref":"#"},"examples":[{"$ref":"#"}]}`,
			want:   `{"const":{"$ref":"#"},"enum":[{"$dynamicRef":"#/items"}],"default":{"$id":"other","$ref":"#"},"examples":[{"$ref":"#"}]}`,
		},
		{
			name:   "keyword names in schema maps",
			schema: `{"properties":{"const":{"$ref":"#"},"$id":{"$ref":"#"}},"patternProperties":{"enum":{"$ref":"#"}},"$defs":{"default":{"$ref":"#"}},"definitions":{"examples":{"$ref":"#"}},"dependentSchemas":{"$ref":{"$ref":"#"}},"dependencies":{"const":{"$ref":"#"},"array":["$ref","#"]}}`,
			want:   `{"properties":{"const":{"$ref":"#/properties/result"},"$id":{"$ref":"#/properties/result"}},"patternProperties":{"enum":{"$ref":"#/properties/result"}},"$defs":{"default":{"$ref":"#/properties/result"}},"definitions":{"examples":{"$ref":"#/properties/result"}},"dependentSchemas":{"$ref":{"$ref":"#/properties/result"}},"dependencies":{"const":{"$ref":"#/properties/result"},"array":["$ref","#"]}}`,
		},
		{
			name:   "root URI base",
			schema: `{"$id":"https://example.com/schema","$ref":"#/$defs/item","$defs":{"item":{"$ref":"#"}}}`,
			want:   `{"$id":"https://example.com/schema","$ref":"#/$defs/item","$defs":{"item":{"$ref":"#"}}}`,
		},
		{
			name:   "nested URI base and fragment base",
			schema: `{"$id":"#root","anyOf":[{"$id":"child.json","$ref":"#"},{"$id":"#child","$ref":"#"},{"$id":"","$ref":"#"}]}`,
			want:   `{"$id":"#root","anyOf":[{"$id":"child.json","$ref":"#"},{"$id":"#child","$ref":"#/properties/result"},{"$id":"","$ref":"#"}]}`,
		},
		{
			name:   "dialect hoisted",
			schema: `{"$schema":"http://json-schema.org/draft-07/schema#","type":"array","items":[{"type":"string"}]}`,
			want:   `{"$schema":"http://json-schema.org/draft-07/schema#","type":"array","items":[{"type":"string"}]}`,
		},
		{
			name:   "2019 static recursive reference",
			schema: `{"$schema":"https://json-schema.org/draft/2019-09/schema","items":{"$recursiveRef":"#"}}`,
			want:   `{"$schema":"https://json-schema.org/draft/2019-09/schema","items":{"$ref":"#/properties/result"}}`,
		},
		{
			name:   "2019 recursion conjoined with reference",
			schema: `{"$schema":"https://json-schema.org/draft/2019-09/schema#","$ref":"#/$defs/item","$recursiveRef":"#","allOf":[{"type":"array"}]}`,
			want:   `{"$schema":"https://json-schema.org/draft/2019-09/schema#","$ref":"#/properties/result/$defs/item","allOf":[{"type":"array"},{"$ref":"#/properties/result"}]}`,
		},
		{
			name:   "2019 root dynamic recursion limitation matches SDK",
			schema: `{"$schema":"https://json-schema.org/draft/2019-09/schema","$recursiveAnchor":true,"items":{"$recursiveRef":"#"}}`,
			want:   `{"$schema":"https://json-schema.org/draft/2019-09/schema","$recursiveAnchor":true,"items":{"$recursiveRef":"#"}}`,
		},
		{
			name:   "off dialect recursive reference",
			schema: `{"$recursiveRef":"#"}`,
			want:   `{"$recursiveRef":"#"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var natural, want map[string]any
			if err := json.Unmarshal([]byte(test.schema), &natural); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.want), &want); err != nil {
				t.Fatal(err)
			}
			before := string(mustJSON(t, natural))
			wrapped := wrapOutputSchemaForLegacy(natural)
			if wrapped["type"] != "object" || !reflect.DeepEqual(wrapped["required"], []string{"result"}) {
				t.Fatalf("invalid envelope: %s", mustJSON(t, wrapped))
			}
			if got := wrapped["properties"].(map[string]any)["result"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("relocated schema = %s, want %s", mustJSON(t, got), mustJSON(t, want))
			}
			if dialect, ok := natural["$schema"].(string); ok && wrapped["$schema"] != dialect {
				t.Fatal("dialect was not hoisted to the wrapper")
			}
			if string(mustJSON(t, natural)) != before {
				t.Fatal("original schema was mutated")
			}
		})
	}
}

func TestToolResultSchemaProjection(t *testing.T) {
	tests := []struct {
		name       string
		schema     string
		structured json.RawMessage
		text       bool
		isError    bool
		wrapped    bool
	}{
		{
			name:   "absent structured output",
			schema: `{"type":"string"}`,
		},
		{
			name:       "object without schema",
			structured: json.RawMessage(`{"ok":true}`),
		},
		{
			name:       "true schema with object output",
			schema:     `true`,
			structured: json.RawMessage(`{"ok":true}`),
		},
		{
			name:       "false schema with object output",
			schema:     `false`,
			structured: json.RawMessage(`{"ok":true}`),
		},
		{
			name:       "true schema with array output",
			schema:     `true`,
			structured: json.RawMessage(`[1,2]`),
			wrapped:    true,
		},
		{
			name:       "false schema with primitive output",
			schema:     `false`,
			structured: json.RawMessage(`42`),
			wrapped:    true,
		},
		{
			name:       "explicit object schema",
			schema:     `{"type":"object"}`,
			structured: json.RawMessage(`{"ok":true}`),
		},
		{
			name:       "object under typeless union",
			schema:     `{"oneOf":[{"type":"null"},{"type":"object"}]}`,
			structured: json.RawMessage(`{"result":{"already":"natural"}}`),
			wrapped:    true,
		},
		{
			name:       "schema-less array",
			structured: json.RawMessage(`[1,2,3]`),
			wrapped:    true,
		},
		{
			name:       "primitive with text",
			schema:     `{"type":"string"}`,
			structured: json.RawMessage(`"hello"`),
			text:       true,
			wrapped:    true,
		},
		{
			name:       "false",
			structured: json.RawMessage(`false`),
			wrapped:    true,
		},
		{
			name:       "zero",
			structured: json.RawMessage(`0`),
			wrapped:    true,
		},
		{
			name:       "explicit null",
			structured: json.RawMessage(`null`),
			wrapped:    true,
		},
		{
			name:       "tool error with structured output",
			schema:     `{"type":"array"}`,
			structured: json.RawMessage(`[]`),
			isError:    true,
			wrapped:    true,
		},
	}
	for _, test := range tests {
		for _, modern := range []bool{false, true} {
			name := test.name + "/legacy"
			if modern {
				name = test.name + "/modern"
			}
			t.Run(name, func(t *testing.T) {
				var schema any
				if test.schema != "" {
					if err := json.Unmarshal([]byte(test.schema), &schema); err != nil {
						t.Fatal(err)
					}
				}
				request := &mcp.ServerRequest[*mcp.CallToolParams]{Params: &mcp.CallToolParams{}}
				if modern {
					request.Params.Meta = mcp.Meta{mcp.MetaKeyProtocolVersion: currentProtocolVersion}
				}
				original := &mcp.CallToolResult{
					Meta:    mcp.Meta{"custom": "preserved"},
					Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: []byte{1}}},
					IsError: test.isError,
				}
				if test.structured != nil {
					original.StructuredContent = test.structured
				}
				if test.text {
					original.Content = append(original.Content, &mcp.TextContent{Text: "authored"})
				}
				before := string(mustJSON(t, original))
				result, err := normalizeCallToolResult(request, original, schema)
				if err != nil {
					t.Fatal(err)
				}
				wire := marshalWireResult(t, result)
				raw, present := wire["structuredContent"]
				if present != (test.structured != nil) {
					t.Fatalf("structured content presence changed: %s", mustJSON(t, result))
				}
				if present {
					want := test.structured
					if test.wrapped && !modern {
						want = append(append([]byte(`{"result":`), want...), '}')
					}
					if string(raw) != string(want) {
						t.Fatalf("structured content = %s, want %s", raw, want)
					}
				}
				wantLength := len(original.Content)
				nonObject := test.structured != nil && test.structured[0] != '{'
				if nonObject && !test.text {
					wantLength++
					if text := result.Content[len(result.Content)-1].(*mcp.TextContent).Text; text != string(test.structured) {
						t.Fatalf("fallback text = %s, want %s", text, test.structured)
					}
				}
				if len(result.Content) != wantLength || result.IsError != original.IsError || !reflect.DeepEqual(result.Meta, original.Meta) {
					t.Fatalf("result fields changed: %s", mustJSON(t, result))
				}
				if _, ok := result.Content[0].(*mcp.ImageContent); !ok {
					t.Fatal("original content block was lost")
				}
				if string(mustJSON(t, original)) != before {
					t.Fatal("original result was mutated")
				}
			})
		}
	}
}

func TestProjectedSchemaValidatesNaturalValues(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		value  any
	}{
		{
			name:   "local definition",
			schema: `{"$ref":"#/$defs/item","$defs":{"item":{"type":"array","items":{"type":"integer"}}}}`,
			value:  []any{float64(1), float64(2)},
		},
		{
			name:   "nullable union",
			schema: `{"oneOf":[{"type":"null"},{"type":"object"}]}`,
			value:  nil,
		},
		{
			name:   "primitive",
			schema: `{"type":"string"}`,
			value:  "hello",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var natural map[string]any
			if err := json.Unmarshal([]byte(test.schema), &natural); err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(mustJSON(t, wrapOutputSchemaForLegacy(natural)), &schema); err != nil {
				t.Fatal(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(map[string]any{"result": test.value}); err != nil {
				t.Fatalf("projected result does not satisfy advertised schema: %v", err)
			}
			if err := resolved.Validate(map[string]any{}); err == nil {
				t.Fatal("envelope accepted a missing result")
			}
		})
	}
}

func TestProjectToolsPreservesCatalog(t *testing.T) {
	typeless := &mcp.Tool{Name: "typeless", InputSchema: map[string]any{"type": "object"}, OutputSchema: json.RawMessage(`{"properties":{"value":{"type":"string"}}}`)}
	object := &mcp.Tool{Name: "object", OutputSchema: &jsonschema.Schema{Type: "object"}}
	absent := &mcp.Tool{Name: "absent"}
	original := []*mcp.Tool{typeless, object, absent}
	before := string(mustJSON(t, original))
	legacy := &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{}}
	projected, err := projectTools(legacy, original)
	if err != nil {
		t.Fatal(err)
	}
	if projected[0] == typeless || projected[1] != object || projected[2] != absent {
		t.Fatal("copy-on-write did not preserve unaffected definitions")
	}
	if string(mustJSON(t, original)) != before {
		t.Fatal("original catalog was mutated")
	}
	again, err := projectTools(legacy, projected)
	if err != nil || string(mustJSON(t, again)) != string(mustJSON(t, projected)) {
		t.Fatal("schema was double wrapped")
	}
	modern := &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{Meta: mcp.Meta{mcp.MetaKeyProtocolVersion: currentProtocolVersion}}}
	current, err := projectTools(modern, original)
	if err != nil || current[0] != typeless {
		t.Fatal("modern definitions were projected")
	}
}

func TestProjectToolsPassesThroughNonObjectSchemas(t *testing.T) {
	for _, raw := range []string{`true`, `false`, `"unknown"`, `[]`, `null`} {
		t.Run(raw, func(t *testing.T) {
			tools := []*mcp.Tool{
				{
					Name:         "unusual",
					InputSchema:  map[string]any{"type": "object"},
					OutputSchema: json.RawMessage(raw),
				},
				{
					Name:         "valid",
					InputSchema:  map[string]any{"type": "object"},
					OutputSchema: map[string]any{"type": "object"},
				},
			}
			before := string(mustJSON(t, tools))
			request := &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{}}
			projected, err := projectTools(request, tools)
			if err != nil {
				t.Fatalf("one non-object schema broke listing: %v", err)
			}
			if len(projected) != 2 || projected[0] != tools[0] || projected[1] != tools[1] {
				t.Fatal("non-object schema affected other tools or caused wrapping")
			}
			if string(mustJSON(t, projected)) != before || string(mustJSON(t, tools)) != before {
				t.Fatal("schema pass-through changed the listing or catalog")
			}
		})
	}
}
