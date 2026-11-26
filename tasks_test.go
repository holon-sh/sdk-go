package holon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	holon "github.com/holon-sh/sdk-go"
)

func taskJSON(id, status, fields string) string {
	return `{"taskId":"` + id + `","status":"` + status + `","statusMessage":"Round 1","createdAt":"2026-11-26T15:04:05Z",` +
		`"lastUpdatedAt":"2026-11-26T15:04:05Z","ttlMs":7200000,"pollIntervalMs":2000` + fields + `}`
}

type taskRequest struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}

type rawResult struct {
	mcp.ResultBase
	body string
}

func (r *rawResult) MarshalJSON() ([]byte, error) { return []byte(r.body), nil }

func withResultType(resultType, task string) *rawResult {
	return &rawResult{body: `{"resultType":"` + resultType + `",` + strings.TrimPrefix(task, "{")}
}

const tasksCapability = "io.modelcontextprotocol/tasks"

func startTaskPeer(t *testing.T, created string, tasks map[string]string) func() []string {
	peer := mcp.NewServer(&mcp.Implementation{Name: "peer", Version: "1"}, nil)
	mcp.AddTool(peer, &mcp.Tool{Name: "work"}, func(context.Context, *mcp.CallToolRequest, struct {
		Direct bool `json:"direct"`
		JSON   bool `json:"json"`
	}) (*mcp.CallToolResult, map[string]string, error) {
		return nil, map[string]string{"answer": "direct"}, nil
	})
	peer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			call, ok := request.(*mcp.CallToolRequest)
			if !ok || call.ClientCapabilities().Extensions[tasksCapability] == nil || strings.Contains(string(call.Params.Arguments), `"direct":true`) {
				return next(ctx, method, request)
			}
			return withResultType("task", created), nil
		}
	})
	acknowledge := func(context.Context, *mcp.ServerSession, *taskRequest) (*rawResult, error) {
		return &rawResult{body: `{"resultType":"complete"}`}, nil
	}
	if err := errors.Join(
		mcp.AddReceivingCustomMethod(peer, "tasks/get", func(_ context.Context, _ *mcp.ServerSession, params *taskRequest) (*rawResult, error) {
			task, ok := tasks[params.TaskID]
			if !ok {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Task not found"}
			}
			return withResultType("complete", task), nil
		}),
		mcp.AddReceivingCustomMethod(peer, "tasks/update", acknowledge),
		mcp.AddReceivingCustomMethod(peer, "tasks/cancel", acknowledge),
	); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var received []string
	events := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer }, &mcp.StreamableHTTPOptions{Stateless: true})
	bodies := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var message struct {
			Method string `json:"method"`
			Params struct {
				Meta struct {
					Capabilities struct {
						Extensions map[string]json.RawMessage `json:"extensions"`
					} `json:"io.modelcontextprotocol/clientCapabilities"`
				} `json:"_meta"`
				InputResponses json.RawMessage `json:"inputResponses"`
			} `json:"params"`
		}
		json.Unmarshal(body, &message)
		_, declared := message.Params.Meta.Capabilities.Extensions[tasksCapability]
		mu.Lock()
		received = append(received, strings.TrimSpace(fmt.Sprintf("%s %s declared=%t %s", message.Method, r.Header.Get("Mcp-Name"), declared, string(message.Params.InputResponses))))
		mu.Unlock()
		if bytes.Contains(body, []byte(`"json":true`)) {
			bodies.ServeHTTP(w, r)
			return
		}
		events.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("PEER_MCP_URL", server.URL+"/mcp")
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		defer func() { received = nil }()
		return received
	}
}

func TestTasks(t *testing.T) {
	const (
		completedResult = `{"content":[{"type":"text","text":"done"}],"structuredContent":{"answer":"task"}}`
		question        = `{"size":{"method":"elicitation/create","params":{"mode":"form","message":"Which size?","requestedSchema":{"type":"object"}}}}`
		failure         = `{"code":-32001,"message":"peer rejected","data":{"reason":"kept"}}`
	)
	created := taskJSON("created", "working", "")
	tasks := map[string]string{
		"working":   taskJSON("working", "working", ""),
		"asking":    taskJSON("asking", "input_required", `,"inputRequests":`+question),
		"completed": taskJSON("completed", "completed", `,"result":`+completedResult),
		"failed":    taskJSON("failed", "failed", `,"error":`+failure),
		"cancelled": taskJSON("cancelled", "cancelled", ""),
	}
	received := startTaskPeer(t, created, tasks)
	worker := &holon.Dependency{Name: "peer", Tools: map[string]holon.ToolSchema{"work": {Input: `{"type":"object"}`}}}
	work := func(direct, json bool) *mcp.CallToolParams {
		return &mcp.CallToolParams{Name: "work", Arguments: map[string]any{"direct": direct, "json": json}}
	}
	answer := mcp.InputResponseMap{"size": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"size": "M"}}}
	for _, test := range []struct {
		name     string
		call     func(context.Context) (any, error)
		kind     string
		json     string
		received []string
	}{
		{
			name: "an ordinary call to a server of tasks", kind: "*mcp.CallToolResult", json: `{"answer":"direct"}`,
			call:     func(ctx context.Context) (any, error) { return worker.CallTool(ctx, work(false, false)) },
			received: []string{"tools/call work declared=false"},
		},
		{
			name: "a task", kind: "holon.Created", json: created,
			call:     func(ctx context.Context) (any, error) { return worker.StartTask(ctx, work(false, false)) },
			received: []string{"tools/call work declared=true"},
		},
		{
			name: "a task in a JSON response", kind: "holon.Created", json: created,
			call:     func(ctx context.Context) (any, error) { return worker.StartTask(ctx, work(false, true)) },
			received: []string{"tools/call work declared=true"},
		},
		{
			name: "a direct answer to a call that may become a task", kind: "holon.Answered", json: `{"answer":"direct"}`,
			call:     func(ctx context.Context) (any, error) { return worker.StartTask(ctx, work(true, false)) },
			received: []string{"tools/call work declared=true"},
		},
		{
			name: "a working task", kind: "holon.WorkingTask", json: tasks["working"],
			call:     func(ctx context.Context) (any, error) { return worker.GetTask(ctx, "working") },
			received: []string{"tasks/get working declared=true"},
		},
		{
			name: "a task that asks", kind: "holon.InputRequiredTask", json: tasks["asking"],
			call:     func(ctx context.Context) (any, error) { return worker.GetTask(ctx, "asking") },
			received: []string{"tasks/get asking declared=true"},
		},
		{
			name: "a completed task", kind: "holon.CompletedTask", json: tasks["completed"],
			call:     func(ctx context.Context) (any, error) { return worker.GetTask(ctx, "completed") },
			received: []string{"tasks/get completed declared=true"},
		},
		{
			name: "a failed task", kind: "holon.FailedTask", json: tasks["failed"],
			call:     func(ctx context.Context) (any, error) { return worker.GetTask(ctx, "failed") },
			received: []string{"tasks/get failed declared=true"},
		},
		{
			name: "a cancelled task", kind: "holon.CancelledTask", json: tasks["cancelled"],
			call:     func(ctx context.Context) (any, error) { return worker.GetTask(ctx, "cancelled") },
			received: []string{"tasks/get cancelled declared=true"},
		},
		{
			name: "an answer to a task's question", kind: "<nil>", json: "null",
			call:     func(ctx context.Context) (any, error) { return nil, worker.UpdateTask(ctx, "asking", answer) },
			received: []string{`tasks/update asking declared=true {"size":{"action":"accept","content":{"size":"M"}}}`},
		},
		{
			name: "a cancelled call", kind: "<nil>", json: "null",
			call:     func(ctx context.Context) (any, error) { return nil, worker.CancelTask(ctx, "working") },
			received: []string{"tasks/cancel working declared=true"},
		},
		{
			name: "an unknown task", kind: "error", json: `{"code":-32602,"message":"Task not found"}`,
			call:     func(ctx context.Context) (any, error) { return worker.GetTask(ctx, "unknown") },
			received: []string{"tasks/get unknown declared=true"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.call(t.Context())
			if kind, data := describe(got, err); kind != test.kind || !sameJSON(data, test.json) {
				t.Errorf("got %s %s, want %s %s", kind, data, test.kind, test.json)
			}
			if got := received(); !slices.Equal(got, test.received) {
				t.Errorf("peer received %q, want %q", got, test.received)
			}
		})
	}
}

func describe(value any, err error) (string, string) {
	kind := fmt.Sprintf("%T", value)
	var rpcError *jsonrpc.Error
	switch {
	case errors.As(err, &rpcError):
		kind, value = "error", rpcError
	case err != nil:
		return "error", err.Error()
	}
	switch v := value.(type) {
	case *mcp.CallToolResult:
		value = v.StructuredContent
	case holon.Answered:
		value = v.Result.StructuredContent
	}
	data, _ := json.Marshal(value)
	return kind, string(data)
}

func sameJSON(got, want string) bool {
	var a, b any
	return json.Unmarshal([]byte(got), &a) == nil && json.Unmarshal([]byte(want), &b) == nil && reflect.DeepEqual(a, b)
}
