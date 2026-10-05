// Package toolsearch indexes allowed MCP tools and implements search tool responses.
package toolsearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// SearchToolName is the public MCP tool used to find available tools.
	SearchToolName = "mmmcp_search_tools"
	// CallToolName is the public MCP tool used to invoke a search result.
	CallToolName = "mmmcp_call_tool"
	searchWait   = 30 * time.Second
)

var (
	// ErrNotReady indicates that the index did not become ready before the wait ended.
	ErrNotReady             = errors.New("tool search index is not ready")
	errInvalidCallArguments = errors.New("tool and revision are required; arguments must be an object when provided")

	camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	separators    = strings.NewReplacer("_", " ", "-", " ", "/", " ", ".", " ")
)

// Reference identifies a tool by its unique exposed name.
type Reference struct {
	Name string `json:"name"`
}

// Document contains the effective tool definition and identity to index.
type Document struct {
	ExposedName string
	Component   string
	Reference   Reference
	Revision    string
	Tool        *mcp.Tool
}

// Hit is a matching tool with the information needed to invoke it.
type Hit struct {
	Reference Reference `json:"tool"`
	Revision  string    `json:"revision"`
	Component string    `json:"component"`
	Tool      *mcp.Tool `json:"definition"`
}

// Results contains ranked search hits and indicates whether more matches exist.
type Results struct {
	Tools   []Hit `json:"tools"`
	HasMore bool  `json:"hasMore"`
}

// CallArguments is the input accepted by the generic MCP call tool.
type CallArguments struct {
	Tool      Reference       `json:"tool"`
	Revision  string          `json:"revision"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Index owns one immutable snapshot's index and its background builder.
type Index struct {
	documents   map[string]Document
	mu          sync.RWMutex
	index       bleve.Index
	ready       chan struct{}
	stopped     chan struct{}
	once        sync.Once
	cancel      context.CancelFunc
	build       func(context.Context, map[string]Document) (bleve.Index, error)
	waitTimeout time.Duration
	retryDelay  time.Duration
}

// IsToolCall reports whether name belongs to a tool implemented by toolsearch.
func IsToolCall(name string) bool {
	return name == SearchToolName || name == CallToolName
}

// ParseCallArguments validates a generic tool call and defaults omitted arguments to an empty object.
func ParseCallArguments(arguments any) (CallArguments, error) {
	var args CallArguments
	data, err := json.Marshal(arguments)
	if err != nil {
		return CallArguments{}, errInvalidCallArguments
	}

	if err := json.Unmarshal(data, &args); err != nil || args.Tool.Name == "" || args.Revision == "" {
		return CallArguments{}, errInvalidCallArguments
	}

	if len(args.Arguments) == 0 {
		args.Arguments = json.RawMessage(`{}`)
	} else {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(args.Arguments, &object); err != nil || object == nil {
			return CallArguments{}, errInvalidCallArguments
		}
	}
	return args, nil
}

// Definitions returns the search and generic call MCP tool definitions.
func Definitions() []*mcp.Tool {
	return []*mcp.Tool{
		{
			Name:        SearchToolName,
			Description: "Find available tools by name, description, and input parameters. Results include schemas and references for mmmcp_call_tool. Use offset to retrieve further results when hasMore is true.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Natural-language tool search query",
					},
					"limit": map[string]any{
						"type":    "integer",
						"minimum": 1,
						"default": 5,
					},
					"offset": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"default":     0,
						"description": "Number of ranked results to skip",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        CallToolName,
			Description: "Invoke a tool returned by mmmcp_search_tools using its reference and revision.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]any{
								"type": "string",
							},
						},
						"required": []string{"name"},
					},
					"revision": map[string]any{
						"type": "string",
					},
					"arguments": map[string]any{
						"type": "object",
					},
				},
				"required": []string{"tool", "revision"},
			},
		},
	}
}

// Revision identifies an exposed tool definition and its source tool.
// Its inputs must be safe to expose to clients: component connection settings
// may contain credentials and must not contribute to a client-visible digest.
func Revision(ref Reference, componentName, originalToolName string, tool *mcp.Tool) string {
	data, _ := json.Marshal([]any{ref, componentName, originalToolName, tool})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// New creates an unbuilt index for one allowed-tool snapshot.
func New(documents []Document) *Index {
	byName := make(map[string]Document, len(documents))
	for _, document := range documents {
		byName[document.ExposedName] = document
	}

	state := &Index{
		documents:   byName,
		ready:       make(chan struct{}),
		stopped:     make(chan struct{}),
		build:       buildIndex,
		waitTimeout: searchWait,
		retryDelay:  time.Second,
	}

	runtime.SetFinalizer(state, func(index *Index) {
		index.mu.RLock()
		defer index.mu.RUnlock()
		if index.index != nil {
			_ = index.index.Close()
		}
	})

	return state
}

// Start builds in the background and retries failed attempts until stopped.
func (i *Index) Start(ctx context.Context) {
	i.once.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		i.mu.Lock()
		i.cancel = cancel
		i.mu.Unlock()

		go i.run(workerCtx)
	})
}

func (i *Index) run(ctx context.Context) {
	defer close(i.stopped)
	defer func() {
		i.mu.Lock()
		if i.cancel != nil {
			i.cancel()
		}
		i.mu.Unlock()
	}()

	delay := i.retryDelay
	for ctx.Err() == nil {
		index, err := i.build(ctx, i.documents)
		if err == nil {
			if ctx.Err() != nil {
				_ = index.Close()
				return
			}

			i.publish(index)
			return
		}
		if ctx.Err() != nil {
			return
		}

		slog.Error("tool search index build failed; retrying", "error", err, "retryAfter", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		delay = min(delay*2, 30*time.Second)
	}
}

func (i *Index) publish(index bleve.Index) {
	i.mu.Lock()
	i.index = index
	close(i.ready)
	i.mu.Unlock()
}

// Stop cancels pending work. A published index remains usable by in-flight requests.
func (i *Index) Stop() {
	if i == nil {
		return
	}
	i.mu.RLock()
	cancel := i.cancel
	i.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (i *Index) await(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-i.ready:
		return nil
	case <-i.stopped:
		select {
		case <-i.ready:
			return nil
		default:
			return ErrNotReady
		}
	case <-timer.C:
		return ErrNotReady
	case <-ctx.Done():
		return ctx.Err()
	}
}

func searchText(value string) string {
	splitCamelCase := camelBoundary.ReplaceAllString(value, "$1 $2")
	splitSeparators := separators.Replace(splitCamelCase)
	return strings.ToLower(value) + " " + strings.ToLower(splitSeparators)
}

func schemaTerms(value any) string {
	var schema any
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		return ""
	}

	var parts []string
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 12 || len(parts) > 256 {
			return
		}
		switch value := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(value))
			for key := range value {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				child := value[key]
				switch key {
				case "properties", "$defs", "definitions":
					if fields, ok := child.(map[string]any); ok {
						fieldNames := make([]string, 0, len(fields))
						for name := range fields {
							fieldNames = append(fieldNames, name)
						}
						slices.Sort(fieldNames)
						for _, name := range fieldNames {
							parts = append(parts, searchText(name))
							walk(fields[name], depth+1)
						}
					}
				case "title", "description":
					if text, ok := child.(string); ok {
						parts = append(parts, text)
					}
				case "items", "anyOf", "oneOf", "allOf":
					walk(child, depth+1)
				}
			}
		case []any:
			for _, child := range value {
				walk(child, depth+1)
			}
		}
	}
	walk(schema, 0)
	return strings.Join(parts, " ")
}

func buildIndex(ctx context.Context, documents map[string]Document) (bleve.Index, error) {
	mapping := bleve.NewIndexMapping()
	mapping.ScoringModel = "bm25"
	mapping.DefaultAnalyzer = "en"
	mapping.DefaultMapping.Dynamic = false
	for _, field := range []string{"name", "component", "description", "parameters"} {
		mapping.DefaultMapping.AddFieldMappingsAt(field, bleve.NewTextFieldMapping())
	}

	index, err := bleve.NewUsing("", mapping, bleve.Config.DefaultIndexType, bleve.Config.DefaultMemKVStore, nil)
	if err != nil {
		return nil, fmt.Errorf("create tool search index: %w", err)
	}

	batch := index.NewBatch()
	for _, doc := range documents {
		if err := ctx.Err(); err != nil {
			_ = index.Close()
			return nil, err
		}
		if err := batch.Index(doc.ExposedName, map[string]string{
			"name":        searchText(doc.ExposedName),
			"component":   searchText(doc.Component),
			"description": doc.Tool.Description,
			"parameters":  schemaTerms(doc.Tool.InputSchema),
		}); err != nil {
			_ = index.Close()
			return nil, fmt.Errorf("index tool %q: %w", doc.ExposedName, err)
		}
	}

	if err := index.Batch(batch); err != nil {
		_ = index.Close()
		return nil, fmt.Errorf("build tool search index: %w", err)
	}

	return index, nil
}

// Search waits for a ready index and returns ranked matches after offset.
func (i *Index) Search(ctx context.Context, text string, limit, offset int) (Results, error) {
	if strings.TrimSpace(text) == "" || limit < 1 || offset < 0 {
		return Results{}, fmt.Errorf("query must be nonempty, limit must be positive, and offset must be nonnegative")
	}

	if err := i.await(ctx, i.waitTimeout); err != nil {
		return Results{}, err
	}

	i.mu.RLock()
	index := i.index
	i.mu.RUnlock()
	if offset >= len(i.documents) {
		return Results{Tools: []Hit{}}, nil
	}

	// Fetch the prefix so exact-name promotion happens before taking the page.
	end := offset + min(limit, len(i.documents)-offset)
	fetch := end
	if fetch < len(i.documents) {
		fetch++
	}

	var clauses []query.Query
	fields := []struct {
		name  string
		boost float64
	}{
		{
			name:  "name",
			boost: 4,
		},
		{
			name:  "component",
			boost: 2,
		},
		{
			name:  "description",
			boost: 2,
		},
		{
			name:  "parameters",
			boost: 1,
		},
	}

	for _, field := range fields {
		q := bleve.NewMatchQuery(searchText(text))
		q.SetField(field.name)
		q.SetBoost(field.boost)
		clauses = append(clauses, q)
	}

	request := bleve.NewSearchRequestOptions(bleve.NewDisjunctionQuery(clauses...), fetch, 0, false)
	request.SortBy([]string{"-_score", "_id"})
	result, err := index.SearchInContext(ctx, request)
	if err != nil {
		return Results{}, err
	}

	if _, ok := i.documents[text]; ok {
		found := false
		for _, hit := range result.Hits {
			found = found || hit.ID == text
		}
		if !found {
			result.Hits = append(result.Hits, &search.DocumentMatch{ID: text})
		}
	}

	slices.SortStableFunc(result.Hits, func(a, b *search.DocumentMatch) int {
		if a.ID == text {
			return -1
		}
		if b.ID == text {
			return 1
		}
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})

	response := Results{
		HasMore: len(result.Hits) > end,
		Tools:   make([]Hit, 0, max(0, min(len(result.Hits), end)-offset)),
	}
	for _, found := range result.Hits[min(offset, len(result.Hits)):min(end, len(result.Hits))] {
		doc := i.documents[found.ID]
		response.Tools = append(response.Tools, Hit{
			Reference: doc.Reference,
			Revision:  doc.Revision,
			Component: doc.Component,
			Tool:      doc.Tool,
		})
	}

	return response, nil
}

// Call handles the search MCP tool's arguments and result formatting.
func (i *Index) Call(ctx context.Context, arguments any) (*mcp.CallToolResult, error) {
	var params struct {
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
	}

	data, err := json.Marshal(arguments)
	if err != nil {
		return toolError("INVALID_ARGUMENTS: expected query and optional limit and offset"), nil
	}
	if err := json.Unmarshal(data, &params); err != nil {
		return toolError("INVALID_ARGUMENTS: expected query and optional limit and offset"), nil
	}

	if params.Limit == 0 {
		params.Limit = 5
	}

	if strings.TrimSpace(params.Query) == "" || params.Limit < 1 || params.Offset < 0 {
		return toolError("INVALID_ARGUMENTS: query must be nonempty, limit must be positive, and offset must be nonnegative"), nil
	}

	results, err := i.Search(ctx, params.Query, params.Limit, params.Offset)
	if errors.Is(err, ErrNotReady) {
		return toolError("SEARCH_INDEX_NOT_READY: Tool search is still indexing; retry shortly."), nil
	}
	if err != nil {
		return nil, err
	}

	output, err := json.Marshal(results)
	if err != nil {
		return nil, err
	}

	return &mcp.CallToolResult{
		StructuredContent: results,
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(output)},
		},
	}, nil
}

func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: message},
		},
	}
}
