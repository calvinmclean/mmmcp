package toolsearch

import (
	"context"
	"errors"
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
				ComponentID: "billing-id",
				Name:        "lookup",
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
