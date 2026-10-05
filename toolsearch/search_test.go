package toolsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testIndex() *Index {
	return New([]Document{
		{
			ExposedName: "billing__lookup",
			Component:   "billing",
			Reference: Reference{
				Name: "billing__lookup",
			},
			Revision: "revision",
			Tool: &mcp.Tool{
				Name:        "billing__lookup",
				Description: "Find invoices",
				InputSchema: map[string]any{"type": "object"},
			},
		},
	})
}

func TestSearchText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "camel case",
			input: "getInvoiceByID",
			want:  "getinvoicebyid get invoice by id",
		},
		{
			name:  "underscores",
			input: "billing_find",
			want:  "billing_find billing find",
		},
		{
			name:  "mixed separators",
			input: "list-tools/v2",
			want:  "list-tools/v2 list tools v2",
		},
		{
			name:  "plain name",
			input: "lookup",
			want:  "lookup lookup",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := searchText(tc.input); got != tc.want {
				t.Fatalf("searchText(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSchemaTermsStopsAtWidePropertyLimit(t *testing.T) {
	properties := make(map[string]any, 400)
	for i := range 400 {
		properties[fmt.Sprintf("field_%03d", i)] = map[string]any{"type": "string"}
	}

	terms := schemaTerms(map[string]any{"properties": properties})
	if got := strings.Count(terms, "field_"); got != 256 {
		t.Fatalf("indexed %d property names, want 256", got)
	}
	if !strings.Contains(terms, "field_255") || strings.Contains(terms, "field_256") {
		t.Fatalf("schema terms exceeded the sorted property limit: %q", terms)
	}
}

func TestParseCallArgumentsRequiresObjectWhenProvided(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments string
		want      string
		wantError bool
	}{
		{
			name: "omitted",
			want: `{}`,
		},
		{
			name:      "empty object",
			arguments: `,"arguments":{}`,
			want:      `{}`,
		},
		{
			name:      "object",
			arguments: `,"arguments":{"key":"value"}`,
			want:      `{"key":"value"}`,
		},
		{
			name:      "null",
			arguments: `,"arguments":null`,
			wantError: true,
		},
		{
			name:      "array",
			arguments: `,"arguments":[]`,
			wantError: true,
		},
		{
			name:      "string",
			arguments: `,"arguments":"value"`,
			wantError: true,
		},
		{
			name:      "number",
			arguments: `,"arguments":1`,
			wantError: true,
		},
		{
			name:      "boolean",
			arguments: `,"arguments":false`,
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := json.RawMessage(`{"name":"lookup","revision":"current"` + tc.arguments + `}`)
			parsed, err := ParseCallArguments(input)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "arguments must be an object when provided") {
					t.Fatalf("expected object validation error, got %+v, %v", parsed, err)
				}
				return
			}
			if err != nil || string(parsed.Arguments) != tc.want {
				t.Fatalf("parsed arguments = %q, error = %v; want %q", parsed.Arguments, err, tc.want)
			}
		})
	}
}

func TestSearchToolPagesAllMatches(t *testing.T) {
	documents := make([]Document, 0, 26)
	for n := range 25 {
		name := fmt.Sprintf("tool_%02d", n)
		documents = append(documents, Document{
			ExposedName: name,
			Component:   "fixture",
			Reference:   Reference{Name: name},
			Revision:    "revision",
			Tool:        &mcp.Tool{Name: name, Description: "shared lookup", InputSchema: map[string]any{"type": "object"}},
		})
	}
	documents = append(documents, Document{
		ExposedName: "lookup",
		Component:   "fixture",
		Reference:   Reference{Name: "lookup"},
		Revision:    "revision",
		Tool:        &mcp.Tool{Name: "lookup", Description: "shared lookup", InputSchema: map[string]any{"type": "object"}},
	})
	index := New(documents)
	index.Start(t.Context())
	defer index.Stop()

	seen := map[string]bool{}
	for offset := 0; ; offset += 10 {
		call, err := index.Call(t.Context(), map[string]any{"query": "lookup", "limit": 10, "offset": offset})
		if err != nil || call.IsError {
			t.Fatalf("search at offset %d: %+v, %v", offset, call, err)
		}
		results, ok := call.StructuredContent.(Results)
		if !ok {
			t.Fatalf("structured results: %T", call.StructuredContent)
		}
		var textResults Results
		if err := json.Unmarshal([]byte(call.Content[0].(*mcp.TextContent).Text), &textResults); err != nil || len(textResults.Tools) != len(results.Tools) || textResults.HasMore != results.HasMore {
			t.Fatalf("text and structured results differ: %+v, %+v, %v", textResults, results, err)
		}
		if offset == 0 && (len(results.Tools) == 0 || results.Tools[0].Tool.Name != "lookup") {
			t.Fatalf("exact-name result was not first: %+v", results)
		}
		for _, hit := range results.Tools {
			if seen[hit.Tool.Name] {
				t.Fatalf("duplicate result %q at offset %d", hit.Tool.Name, offset)
			}
			seen[hit.Tool.Name] = true
		}
		if !results.HasMore {
			if len(seen) != len(documents) {
				t.Fatalf("reached end after %d of %d results", len(seen), len(documents))
			}
			break
		}
	}

	call, err := index.Call(t.Context(), map[string]any{"query": "lookup", "limit": 20, "offset": 26})
	if err != nil || call.IsError {
		t.Fatalf("past-end search: %+v, %v", call, err)
	}
	results := call.StructuredContent.(Results)
	if len(results.Tools) != 0 || results.HasMore {
		t.Fatalf("past-end results: %+v", results)
	}

	call, err = index.Call(t.Context(), map[string]any{"query": "lookup", "limit": 26})
	if err != nil || call.IsError {
		t.Fatalf("unrestricted limit search: %+v, %v", call, err)
	}
	results = call.StructuredContent.(Results)
	if len(results.Tools) != len(documents) || results.HasMore {
		t.Fatalf("unrestricted limit results: %+v", results)
	}

	call, err = index.Call(t.Context(), map[string]any{"query": "lookup"})
	if err != nil || call.IsError || len(call.StructuredContent.(Results).Tools) != 5 {
		t.Fatalf("default limit results: %+v, %v", call, err)
	}

	call, err = index.Call(t.Context(), map[string]any{"query": "lookup", "limit": 0})
	if err != nil || call.IsError || len(call.StructuredContent.(Results).Tools) != 5 {
		t.Fatalf("zero limit results: %+v, %v", call, err)
	}
}

func TestSearchToolRejectsNegativeOffset(t *testing.T) {
	call, err := testIndex().Call(t.Context(), map[string]any{"query": "lookup", "offset": -1})
	if err != nil || !call.IsError || !strings.Contains(call.Content[0].(*mcp.TextContent).Text, "offset") {
		t.Fatalf("negative offset: %+v, %v", call, err)
	}
}

func TestSearchToolDefinitionAllowsUnboundedPositiveLimit(t *testing.T) {
	properties := Definitions()[0].InputSchema.(map[string]any)["properties"].(map[string]any)
	if _, required := Definitions()[0].InputSchema.(map[string]any)["required"]; required {
		t.Fatal("search tool still requires a query")
	}
	if _, ok := properties["name"]; !ok {
		t.Fatal("search tool does not expose exact lookup")
	}
	if _, ok := properties["detail"]; ok {
		t.Fatal("search tool still exposes browse detail")
	}
	limit := properties["limit"].(map[string]any)
	if _, capped := limit["maximum"]; capped {
		t.Fatalf("limit schema still has a maximum: %+v", limit)
	}
	if offset := properties["offset"].(map[string]any); offset["minimum"] != 0 {
		t.Fatalf("offset schema: %+v", offset)
	}
}

func TestBrowseAndExactLookupBeforeIndexReady(t *testing.T) {
	index := New([]Document{
		{ExposedName: "github__get_issue", Component: "github", Reference: Reference{Name: "github__get_issue"}, Revision: "get-revision", Tool: &mcp.Tool{Name: "github__get_issue", Description: "Get an issue", InputSchema: map[string]any{"type": "object"}}},
		{ExposedName: "gmail__send", Component: "gmail", Reference: Reference{Name: "gmail__send"}, Revision: "send-revision", Tool: &mcp.Tool{Name: "gmail__send", InputSchema: map[string]any{"type": "object"}}},
		{ExposedName: "github__create_issue", Component: "github", Reference: Reference{Name: "github__create_issue"}, Revision: "create-revision", Tool: &mcp.Tool{Name: "github__create_issue", InputSchema: map[string]any{"type": "object"}}},
	})

	call := func(arguments any) *mcp.CallToolResult {
		t.Helper()
		result, err := index.Call(t.Context(), arguments)
		if err != nil || result.IsError {
			t.Fatalf("call(%v) = %+v, %v", arguments, result, err)
		}
		wire, err := json.Marshal(result.StructuredContent)
		if err != nil || result.Content[0].(*mcp.TextContent).Text != string(wire) {
			t.Fatalf("text and structured results differ: %s, %v", wire, err)
		}
		return result
	}

	for _, arguments := range []any{nil, map[string]any{}} {
		browse := call(arguments).StructuredContent.(BrowseResults)
		if len(browse.Components) != 2 || browse.Components[0].Name != "github" || browse.Components[0].ToolCount != 2 || browse.Components[1].Name != "gmail" || browse.Components[1].ToolCount != 1 {
			t.Fatalf("browse(%v) = %+v", arguments, browse)
		}
		if got := browse.Components[0].Tools; len(got) != 2 || got[0] != "github__create_issue" || got[1] != "github__get_issue" {
			t.Fatalf("browse(%v) = %+v", arguments, browse)
		}
		if got := browse.Components[1].Tools; len(got) != 1 || got[0] != "gmail__send" {
			t.Fatalf("browse(%v) = %+v", arguments, browse)
		}
	}

	lookup := call(map[string]any{"name": "github__get_issue"}).StructuredContent.(Results)
	if lookup.HasMore || len(lookup.Tools) != 1 || lookup.Tools[0].Revision != "get-revision" || lookup.Tools[0].Tool.InputSchema == nil {
		t.Fatalf("exact lookup = %+v", lookup)
	}
	missing := call(map[string]any{"name": "get_issue"}).StructuredContent.(Results)
	if missing.HasMore || len(missing.Tools) != 0 {
		t.Fatalf("non-exact lookup = %+v", missing)
	}
}

func TestSearchModesRejectInvalidArguments(t *testing.T) {
	index := testIndex()
	for _, arguments := range []any{
		map[string]any{"query": ""},
		map[string]any{"name": ""},
		map[string]any{"query": "lookup", "name": "billing__lookup"},
		map[string]any{"limit": 2},
		map[string]any{"name": "billing__lookup", "offset": 1},
		map[string]any{"name": nil},
		[]any{},
	} {
		result, err := index.Call(t.Context(), arguments)
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "INVALID_ARGUMENTS") {
			t.Fatalf("call(%v) = %+v, %v", arguments, result, err)
		}
	}
}

func TestAsyncIndexWaitTimeoutAndConcurrentSearch(t *testing.T) {
	index := testIndex()
	started := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	index.build = func(ctx context.Context, docs map[string]Document) (bleve.Index, error) {
		attempts.Add(1)
		close(started)
		select {
		case <-release:
			return buildIndex(ctx, docs)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	index.waitTimeout = 10 * time.Millisecond

	startReturned := make(chan struct{})
	go func() {
		index.Start(t.Context())
		close(startReturned)
	}()
	select {
	case <-startReturned:
	case <-time.After(time.Second):
		t.Fatal("starting an index blocked on its build")
	}
	<-started

	result, err := index.Call(t.Context(), map[string]any{"query": "invoices"})
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "SEARCH_INDEX_NOT_READY") {
		t.Fatalf("pending search = %+v, %v", result, err)
	}
	index.waitTimeout = time.Second

	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			found, err := index.Search(t.Context(), "invoices", 5, 0)
			if err != nil || len(found.Tools) != 1 {
				t.Errorf("search after build = %+v, %v", found, err)
			}
		})
	}
	close(release)
	workers.Wait()

	if attempts.Load() != 1 {
		t.Fatalf("build attempts = %d, want one", attempts.Load())
	}
	index.Stop()
}

func TestAsyncIndexRetriesFailedBuild(t *testing.T) {
	index := testIndex()
	index.retryDelay = time.Millisecond
	var attempts atomic.Int32
	index.build = func(ctx context.Context, docs map[string]Document) (bleve.Index, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("temporary build failure")
		}
		return buildIndex(ctx, docs)
	}

	index.Start(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	found, err := index.Search(ctx, "invoices", 5, 0)
	if err != nil || len(found.Tools) != 1 || attempts.Load() != 2 {
		t.Fatalf("search after retry = %+v, attempts = %d, err = %v", found, attempts.Load(), err)
	}
	index.Stop()
}

func TestAsyncIndexStopsPendingBuild(t *testing.T) {
	index := testIndex()
	started := make(chan struct{})
	index.build = func(ctx context.Context, _ map[string]Document) (bleve.Index, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	index.Start(t.Context())
	<-started
	index.Stop()

	select {
	case <-index.stopped:
	case <-time.After(time.Second):
		t.Fatal("index builder did not stop")
	}

	if _, err := index.Search(t.Context(), "invoices", 5, 0); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stopped search error = %v", err)
	}
}
