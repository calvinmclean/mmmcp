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
	if err := index.Build(t.Context()); err != nil {
		t.Fatal(err)
	}

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
}

func TestSearchToolRejectsNegativeOffset(t *testing.T) {
	call, err := testIndex().Call(t.Context(), map[string]any{"query": "lookup", "offset": -1})
	if err != nil || !call.IsError || !strings.Contains(call.Content[0].(*mcp.TextContent).Text, "offset") {
		t.Fatalf("negative offset: %+v, %v", call, err)
	}
}

func TestSearchToolDefinitionAllowsUnboundedPositiveLimit(t *testing.T) {
	properties := Definitions()[0].InputSchema.(map[string]any)["properties"].(map[string]any)
	limit := properties["limit"].(map[string]any)
	if _, capped := limit["maximum"]; capped {
		t.Fatalf("limit schema still has a maximum: %+v", limit)
	}
	if offset := properties["offset"].(map[string]any); offset["minimum"] != 0 {
		t.Fatalf("offset schema: %+v", offset)
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
			found, err := index.Search(t.Context(), "invoices", 5)
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

	found, err := index.Search(ctx, "invoices", 5)
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

	if _, err := index.Search(t.Context(), "invoices", 5); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stopped search error = %v", err)
	}
}
