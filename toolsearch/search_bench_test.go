package toolsearch

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BenchmarkBuildIndex measures a fresh index build for each catalog snapshot.
// Fixtures are created outside the timer; index creation and batch indexing are
// included, while closing the completed index is excluded.
func BenchmarkBuildIndex(b *testing.B) {
	cases := []struct {
		name        string
		tools       int
		fields      int
		description int
		components  int
		schemaDepth int
	}{
		{
			name:        "tools=10",
			tools:       10,
			fields:      2,
			description: 1,
			components:  1,
		},
		{
			name:        "tools=100",
			tools:       100,
			fields:      2,
			description: 1,
			components:  1,
		},
		{
			name:        "tools=1000",
			tools:       1000,
			fields:      2,
			description: 1,
			components:  1,
		},
		{
			name:        "tools=5000",
			tools:       5000,
			fields:      2,
			description: 1,
			components:  1,
		},

		{
			name:        "tools=1000/fields=20",
			tools:       1000,
			fields:      20,
			description: 1,
			components:  1,
		},
		{
			name:        "tools=1000/schema_depth=4",
			tools:       1000,
			fields:      2,
			description: 1,
			components:  1,
			schemaDepth: 4,
		},
		{
			name:        "tools=1000/description=long",
			tools:       1000,
			fields:      2,
			description: 20,
			components:  1,
		},
		{
			name:        "tools=1000/components=100",
			tools:       1000,
			fields:      2,
			description: 1,
			components:  100,
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			documents := benchmarkDocuments(tc.tools, tc.fields, tc.description, tc.components, tc.schemaDepth)

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				index, err := buildIndex(context.Background(), documents)
				if err != nil {
					b.Fatal(err)
				}

				b.StopTimer()
				if err := index.Close(); err != nil {
					b.Fatal(err)
				}

				b.StartTimer()
			}
		})
	}
}

func benchmarkDocuments(count, fields, descriptionRepeats, components, schemaDepth int) map[string]Document {
	documents := make(map[string]Document, count)
	description := strings.Repeat("Find records by name, date, and account. ", descriptionRepeats)

	for n := range count {
		component := fmt.Sprintf("component_%03d", n%components)
		name := fmt.Sprintf("%s__lookup_record_%05d", component, n)

		properties := make(map[string]any, fields)
		for field := range fields {
			properties[fmt.Sprintf("filter_%02d", field)] = map[string]any{
				"type":        "string",
				"description": "Filter records by this value",
			}
		}

		for depth := range schemaDepth {
			properties = map[string]any{
				fmt.Sprintf("nested_%d", depth): map[string]any{
					"type":       "object",
					"properties": properties,
				},
			}
		}

		documents[name] = Document{
			ExposedName: name,
			Component:   component,
			Tool: &mcp.Tool{
				Name:        name,
				Description: description,
				InputSchema: map[string]any{
					"type":       "object",
					"properties": properties,
				},
			},
		}
	}

	return documents
}
