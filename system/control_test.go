package system_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	holon "github.com/holon-sh/sdk-go"
	"github.com/holon-sh/sdk-go/system"
)

// contract is the tools control.json lists.
func contract(t *testing.T) []*mcp.Tool {
	t.Helper()
	data, err := os.ReadFile("control.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []*mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document.Tools
}

// serveControl binds CONTROL_MCP_URL to a server listing control's tools, each of which answers with reply.
func serveControl(t *testing.T, reply func(arguments json.RawMessage) *mcp.CallToolResult) {
	t.Helper()
	control := mcp.NewServer(&mcp.Implementation{Name: "control", Version: "1"}, nil)
	for _, tool := range contract(t) {
		control.AddTool(tool, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return reply(request.Params.Arguments), nil
		})
	}
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return control },
		&mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(server.Close)
	t.Setenv("CONTROL_MCP_URL", server.URL+"/mcp")
}

func TestGetTraces(t *testing.T) {
	var arguments json.RawMessage
	var result *mcp.CallToolResult
	serveControl(t, func(got json.RawMessage) *mcp.CallToolResult {
		arguments = got
		return result
	})
	since := "24h"
	getTraces := func() (system.ControlGetTracesOutput, error) {
		return system.Control.GetTraces(t.Context(), system.ControlGetTracesInput{Since: &since})
	}

	// The traces of a failed call and of a listing, which leaves out what it does not have.
	const traces = `{"app":"reporter","session":"session","traces":[` +
		`{"app":"caller","durationMs":12,"errorCode":-32602,"method":"tools/call","start":"2026-11-18T12:00:00Z","status":"error","tool":"search","traceId":"a1"},` +
		`{"durationMs":3,"method":"tools/list","start":"2026-11-18T11:59:00Z","status":"ok","traceId":"a0"}]}`
	result = &mcp.CallToolResult{StructuredContent: json.RawMessage(traces)}
	page, err := getTraces()
	decoded, _ := json.Marshal(page)
	if err != nil || string(arguments) != `{"since":"24h"}` || string(decoded) != traces ||
		page.Traces[0].TraceId != "a1" || string(page.Traces[0].Status) != `"error"` || page.Traces[1].Tool != nil {
		t.Errorf("page %s from arguments %s: %v", decoded, arguments, err)
	}

	result = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "traces are unavailable"}}}
	_, err = getTraces()
	var failure *holon.ToolCallError
	if !errors.As(err, &failure) || err.Error() != "control.get_traces: tool_error" || !failure.Result.IsError {
		t.Errorf("tool error: %v", err)
	}

	result = &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"session":"session","traces":[
		{"traceId":"a2","start":"2026-11-18T12:01:00Z","method":"tools/call","status":"pending","durationMs":1}]}`)}
	if _, err := getTraces(); err == nil || !strings.Contains(err.Error(), "structured output from control.get_traces") {
		t.Errorf("result outside control.json accepted: %v", err)
	}

	t.Setenv("CONTROL_MCP_URL", "")
	if _, err := getTraces(); err == nil || !strings.Contains(err.Error(), "set CONTROL_MCP_URL") {
		t.Errorf("unbound control: %v", err)
	}
}

// TestTypesFollowContract fails when Control's tools and types differ from those the CLI's Go generator renders for
// control.json.
func TestTypesFollowContract(t *testing.T) {
	types := map[string][2]reflect.Type{
		"get_traces": {reflect.TypeFor[system.ControlGetTracesInput](), reflect.TypeFor[system.ControlGetTracesOutput]()},
	}
	for _, tool := range contract(t) {
		method := reflect.ValueOf(system.Control).MethodByName(identifier(tool.Name))
		schemas, ok := types[tool.Name]
		if !ok || !method.IsValid() || method.Type() != reflect.FuncOf(
			[]reflect.Type{reflect.TypeFor[context.Context](), schemas[0]}, []reflect.Type{schemas[1], reflect.TypeFor[error]()}, false) {
			t.Errorf("no method %s for %s", identifier(tool.Name), tool.Name)
			continue
		}
		for i, schema := range []any{tool.InputSchema, tool.OutputSchema} {
			if got, want := schemas[i].String(), render(t, schema.(map[string]any)); got != want {
				t.Errorf("%s:\n got %s\nwant %s", tool.Name, got, want)
			}
		}
	}
}

var (
	// The words of a name, which the generator capitalizes and joins.
	asciiWords = regexp.MustCompile(`[a-zA-Z0-9]+`)
	// How reflect writes the generator's raw JSON and numbers.
	rawJSON = reflect.TypeFor[json.RawMessage]().String()
	number  = reflect.TypeFor[json.Number]().String()
)

// identifier is the generator's Go name for a tool or property.
func identifier(name string) string {
	words := asciiWords.FindAllString(name, -1)
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, "")
}

// render spells a schema's type as the CLI's Go generator renders it and reflect writes it, for the keywords control
// uses: a property's field is a pointer unless it is required or raw JSON, and an enum of several values is raw JSON.
func render(t *testing.T, schema map[string]any) string {
	t.Helper()
	for keyword, value := range schema {
		known := slices.Contains([]string{"type", "description", "enum", "items", "properties", "required"}, keyword)
		if !known && (keyword != "additionalProperties" || value != false) {
			t.Fatalf("no rendering for %s: %v", keyword, value)
		}
	}
	if enum, _ := schema["enum"].([]any); len(enum) > 1 {
		return rawJSON
	}
	switch schema["type"] {
	case "string":
		return "string"
	case "integer", "number":
		return number
	case "boolean":
		return "bool"
	case "array":
		return "[]" + render(t, schema["items"].(map[string]any))
	case "object":
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		if len(properties) == 0 {
			return "map[string]" + rawJSON
		}
		var fields []string
		for _, name := range slices.Sorted(maps.Keys(properties)) {
			rendered, tag := render(t, properties[name].(map[string]any)), name
			if !slices.Contains(required, any(name)) {
				if rendered != rawJSON {
					rendered = "*" + rendered
				}
				tag += ",omitempty"
			}
			fields = append(fields, fmt.Sprintf("%s %s %q", identifier(name), rendered, `json:"`+tag+`"`))
		}
		return "struct { " + strings.Join(fields, "; ") + " }"
	}
	t.Fatalf("no rendering for %v", schema)
	return ""
}
