//go:build integration

package everything_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/obot-platform/mmmcp/toolsearch"
)

const everythingPackage = "@modelcontextprotocol/server-everything@2026.8.18"

func TestEverythingServerIntegration(t *testing.T) {
	mmmcpBinary := os.Getenv("MMMCP_TEST_BINARY")
	if mmmcpBinary == "" {
		t.Fatal("MMMCP_TEST_BINARY must point to a built mmmcp binary")
	}
	mmmcpBinary, err := filepath.Abs(mmmcpBinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mmmcpBinary); err != nil {
		t.Fatalf("stat mmmcp binary: %v", err)
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Fatal("npx is required for the Everything server integration test")
	}

	testDir := t.TempDir()
	npmCache := filepath.Join(testDir, "npm-cache")
	everythingAddress := freeAddress(t)
	_, everythingPort, err := net.SplitHostPort(everythingAddress)
	if err != nil {
		t.Fatal(err)
	}
	everything := startProcess(npx, "-y", everythingPackage, "streamableHttp")
	everything.cmd.Env = append(os.Environ(), "PORT="+everythingPort, "npm_config_cache="+npmCache)
	everything.start(t)
	defer everything.stop(t)
	waitForTCP(t, everythingAddress, everything)

	for _, toolSearch := range []bool{false, true} {
		mode := "direct"
		if toolSearch {
			mode = "search"
		}
		t.Run(mode, func(t *testing.T) {
			frontendAddress := freeAddress(t)
			configPath := filepath.Join(testDir, "mmmcp-"+mode+".yaml")
			config := fmt.Sprintf(`toolSearch: %t
listen: %s
servers:
  - name: everything-http
    id: everything-http
    prefix: http
    url: %s
  - name: everything-stdio
    id: everything-stdio
    prefix: stdio
    command: %s
    args: [%s, %s]
    env:
      npm_config_cache: %s
`, toolSearch, quote(frontendAddress), quote("http://"+everythingAddress+"/mcp"), quote(npx), quote("-y"), quote(everythingPackage), quote(npmCache))
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}

			t.Run("HTTP frontend", func(t *testing.T) {
				frontend := startProcess(mmmcpBinary,
					"-config", configPath,
					"-listen", frontendAddress,
					"-dsn", filepath.Join(testDir, "mmmcp-"+mode+"-http.db"),
				)
				frontend.start(t)
				defer frontend.stop(t)
				waitForHTTP(t, "http://"+frontendAddress+"/healthz", frontend)

				client := mcp.NewClient(&mcp.Implementation{
					Name:    "everything-http-integration-test",
					Version: "1.0.0",
				}, nil)
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
					Endpoint:             "http://" + frontendAddress,
					DisableStandaloneSSE: true,
				}, nil)
				if err != nil {
					t.Fatalf("connect to mmmcp: %v\n%s", err, frontend.output.String())
				}
				defer session.Close()

				verifyEverythingServer(t, session, toolSearch)
			})

			t.Run("stdio frontend", func(t *testing.T) {
				var stderr lockedBuffer
				command := exec.Command(mmmcpBinary,
					"-config", configPath,
					"-transport", "stdio",
					"-dsn", filepath.Join(testDir, "mmmcp-"+mode+"-stdio.db"),
				)
				command.Stderr = &stderr
				client := mcp.NewClient(&mcp.Implementation{
					Name:    "everything-stdio-integration-test",
					Version: "1.0.0",
				}, nil)
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: 10 * time.Second}, nil)
				if err != nil {
					t.Fatalf("connect to mmmcp: %v\n%s", err, stderr.String())
				}
				defer func() {
					// CommandTransport can reap a cleanly exited process just after its
					// termination timer fires. In that case Close reports ErrProcessDone
					// even though the requested shutdown has completed successfully.
					if err := session.Close(); err != nil && !errors.Is(err, os.ErrProcessDone) {
						t.Errorf("close mmmcp stdio session: %v", err)
					}
					if t.Failed() {
						t.Logf("mmmcp stdio output:\n%s", stderr.String())
					}
				}()

				verifyEverythingServer(t, session, toolSearch)
			})
		})
	}
}

func verifyEverythingServer(t *testing.T, session *mcp.ClientSession, toolSearch bool) {
	t.Helper()
	tools := collectTools(t, session)
	toolNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		toolNames = append(toolNames, tool.Name)
	}
	if toolSearch {
		want := map[string]int{
			toolsearch.SearchToolName: 1,
			toolsearch.CallToolName:   1,
		}
		if !maps.Equal(mapFromNames(toolNames), want) {
			t.Fatalf("search mode tools = %v, want only search and generic call", toolNames)
		}
	} else {
		requirePairedFeatures(t, "tools", toolNames, "http__", "stdio__")
	}

	prompts := collectPrompts(t, session)
	promptNames := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		promptNames = append(promptNames, prompt.Name)
	}
	requirePairedFeatures(t, "prompts", promptNames, "http__", "stdio__")

	resources := collectResources(t, session)
	resourceURIs := make([]string, 0, len(resources))
	for _, resource := range resources {
		resourceURIs = append(resourceURIs, resource.URI)
	}
	requirePairedFeatures(t, "resources", resourceURIs, "mmmcp+http:", "mmmcp+stdio:")

	for _, prefix := range []string{"http", "stdio"} {
		t.Run(prefix+" echo", func(t *testing.T) {
			if toolSearch {
				verifyDirectToolUnavailable(t, session, prefix+"__echo")
				return
			}
			result := callTool(t, session, prefix+"__echo", map[string]any{"message": "mmmcp integration"})
			requireTextResult(t, result, "Echo: mmmcp integration")
		})
		t.Run(prefix+" get-sum", func(t *testing.T) {
			if toolSearch {
				verifyDirectToolUnavailable(t, session, prefix+"__get-sum")
				return
			}
			result := callTool(t, session, prefix+"__get-sum", map[string]any{"a": 2, "b": 3})
			requireTextResult(t, result, "The sum of 2 and 3 is 5.")
		})
	}
	if toolSearch {
		verifyToolSearch(t, session)
	}
}

func mapFromNames(names []string) map[string]int {
	result := make(map[string]int, len(names))
	for _, name := range names {
		result[name]++
	}
	return result
}

func verifyDirectToolUnavailable(t *testing.T, session *mcp.ClientSession, name string) {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      name,
		Arguments: map[string]any{},
	})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatalf("direct call to %q succeeded in search mode: %+v", name, result)
	}
}

func verifyToolSearch(t *testing.T, session *mcp.ClientSession) {
	t.Helper()
	for _, test := range []struct {
		component string
		name      string
		args      map[string]any
		want      string
	}{
		{
			component: "everything-http",
			name:      "echo",
			args:      map[string]any{"message": "searched HTTP"},
			want:      "Echo: searched HTTP",
		},
		{
			component: "everything-stdio",
			name:      "get-sum",
			args: map[string]any{
				"a": 2,
				"b": 3,
			},
			want: "The sum of 2 and 3 is 5.",
		},
	} {
		t.Run(test.component+" search "+test.name, func(t *testing.T) {
			found := callTool(t, session, toolsearch.SearchToolName, map[string]any{
				"query": test.name,
				"limit": 20,
			})
			var results toolsearch.Results
			if len(found.Content) == 0 {
				t.Fatalf("search returned no content: %+v", found)
			}
			content, ok := found.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("search returned non-text content: %+v", found.Content[0])
			}
			if err := json.Unmarshal([]byte(content.Text), &results); err != nil {
				t.Fatalf("decode search result: %v; result = %s", err, resultText(found))
			}

			var hit *toolsearch.Hit
			for i := range results.Tools {
				candidate := &results.Tools[i]
				if candidate.Reference == (toolsearch.Reference{
					ComponentID: test.component,
					Name:        test.name,
				}) {
					hit = candidate
					break
				}
			}
			if hit == nil || hit.Tool == nil || hit.Tool.InputSchema == nil || hit.Revision == "" {
				t.Fatalf("search omitted complete %s/%s definition: %+v", test.component, test.name, results)
			}
			called := callTool(t, session, toolsearch.CallToolName, map[string]any{
				"tool":      hit.Reference,
				"revision":  hit.Revision,
				"arguments": test.args,
			})
			requireTextResult(t, called, test.want)
		})
	}
}

func collectTools(t *testing.T, session *mcp.ClientSession) []*mcp.Tool {
	t.Helper()
	var result []*mcp.Tool
	var cursor string
	for {
		page, err := session.ListTools(t.Context(), &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, page.Tools...)
		if page.NextCursor == "" {
			return result
		}
		cursor = page.NextCursor
	}
}

func collectPrompts(t *testing.T, session *mcp.ClientSession) []*mcp.Prompt {
	t.Helper()
	var result []*mcp.Prompt
	var cursor string
	for {
		page, err := session.ListPrompts(t.Context(), &mcp.ListPromptsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, page.Prompts...)
		if page.NextCursor == "" {
			return result
		}
		cursor = page.NextCursor
	}
}

func collectResources(t *testing.T, session *mcp.ClientSession) []*mcp.Resource {
	t.Helper()
	var result []*mcp.Resource
	var cursor string
	for {
		page, err := session.ListResources(t.Context(), &mcp.ListResourcesParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, page.Resources...)
		if page.NextCursor == "" {
			return result
		}
		cursor = page.NextCursor
	}
}

func requirePairedFeatures(t *testing.T, family string, values []string, firstPrefix, secondPrefix string) {
	t.Helper()
	if len(values) == 0 || len(values)%2 != 0 {
		t.Fatalf("%s count = %d, want a positive even number", family, len(values))
	}
	first := make(map[string]int, len(values)/2)
	second := make(map[string]int, len(values)/2)
	for _, value := range values {
		switch {
		case strings.HasPrefix(value, firstPrefix):
			first[strings.TrimPrefix(value, firstPrefix)]++
		case strings.HasPrefix(value, secondPrefix):
			second[strings.TrimPrefix(value, secondPrefix)]++
		default:
			t.Fatalf("%s identity %q has neither expected prefix", family, value)
		}
	}
	if !maps.Equal(first, second) {
		t.Fatalf("%s differ between HTTP and stdio: HTTP=%v stdio=%v", family, first, second)
	}
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("call %s returned a tool error: %s", name, resultText(result))
	}
	return result
}

func requireTextResult(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok && text.Text == want {
			return
		}
	}
	t.Fatalf("tool result = %q, want text content %q", resultText(result), want)
}

func resultText(result *mcp.CallToolResult) string {
	var values []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			values = append(values, text.Text)
		}
	}
	if result.StructuredContent != nil {
		if data, err := json.Marshal(result.StructuredContent); err == nil {
			values = append(values, string(data))
		}
	}
	return strings.Join(values, "\n")
}

type process struct {
	cmd    *exec.Cmd
	output lockedBuffer
	exited chan struct{}
	mu     sync.Mutex
	err    error
}

func startProcess(name string, args ...string) *process {
	p := &process{cmd: exec.Command(name, args...), exited: make(chan struct{})}
	configureProcessGroup(p.cmd)
	p.cmd.Stdout = &p.output
	p.cmd.Stderr = &p.output
	return p
}

func (p *process) start(t *testing.T) {
	t.Helper()
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", p.cmd.Path, err)
	}
	go func() {
		err := p.cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.exited)
	}()
}

func (p *process) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.exited:
		if t.Failed() {
			t.Logf("%s output:\n%s", p.cmd.Path, p.output.String())
		}
		return
	default:
	}
	_ = signalProcessGroup(p.cmd.Process, os.Interrupt)
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		_ = signalProcessGroup(p.cmd.Process, os.Kill)
		<-p.exited
	}
	if t.Failed() {
		t.Logf("%s output:\n%s", p.cmd.Path, p.output.String())
	}
}

func (p *process) exitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func waitForTCP(t *testing.T, address string, p *process) {
	t.Helper()
	waitFor(t, p, func() bool {
		conn, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

func waitForHTTP(t *testing.T, url string, p *process) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	waitFor(t, p, func() bool {
		response, err := client.Get(url)
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
}

func waitFor(t *testing.T, p *process, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		select {
		case <-p.exited:
			t.Fatalf("%s exited before becoming ready: %v\n%s", p.cmd.Path, p.exitError(), p.output.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("%s did not become ready\n%s", p.cmd.Path, p.output.String())
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func quote(value string) string {
	return strconv.Quote(value)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
