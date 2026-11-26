package holon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#extension-identifier
const tasksExtension = "io.modelcontextprotocol/tasks"

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#polymorphic-results
const resultTypeTask = "task"

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#streamable-http-routing-headers
const nameHeader = "Mcp-Name"

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#task-polling
const (
	methodGetTask    = "tasks/get"
	methodUpdateTask = "tasks/update"
	methodCancelTask = "tasks/cancel"
)

type TaskID string

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#task-status
type TaskStatus string

const (
	TaskWorking       TaskStatus = "working"
	TaskInputRequired TaskStatus = "input_required"
	TaskCompleted     TaskStatus = "completed"
	TaskFailed        TaskStatus = "failed"
	TaskCancelled     TaskStatus = "cancelled"
)

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#tasks-1
type Task struct {
	ID             TaskID     `json:"taskId"`
	Status         TaskStatus `json:"status"`
	StatusMessage  string     `json:"statusMessage,omitempty"`
	CreatedAt      string     `json:"createdAt"`
	LastUpdatedAt  string     `json:"lastUpdatedAt"`
	TTLMs          *int64     `json:"ttlMs"`
	PollIntervalMs *int64     `json:"pollIntervalMs,omitempty"`
}

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#polymorphic-results
type Started interface{ started() }

type Answered struct{ Result *mcp.CallToolResult }

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#task-creation
type Created struct{ Task }

func (Answered) started() {}
func (Created) started()  {}

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#task-status
type DetailedTask interface{ detailed() }

type WorkingTask struct{ Task }

type InputRequiredTask struct {
	Task
	InputRequests mcp.InputRequestMap `json:"inputRequests"`
}

type CompletedTask struct {
	Task
	Result *mcp.CallToolResult `json:"result"`
}

type FailedTask struct {
	Task
	Error *jsonrpc.Error `json:"error"`
}

type CancelledTask struct{ Task }

func (WorkingTask) detailed()       {}
func (InputRequiredTask) detailed() {}
func (CompletedTask) detailed()     {}
func (FailedTask) detailed()        {}
func (CancelledTask) detailed()     {}

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#capability-negotiation
func (d *Dependency) StartTask(ctx context.Context, params *mcp.CallToolParams, elicit ...ElicitHandler) (Started, error) {
	tool, err := d.tool(params)
	if err != nil {
		return nil, err
	}
	var created *Task
	client, err := Connect(ctx, d.Name, tasks, WithTransport(func(next http.RoundTripper) http.RoundTripper {
		return creating{next, &created}
	}))
	if err != nil {
		return nil, err
	}
	defer client.Close()
	result, err := send(ctx, client, *params, elicit)
	if err != nil {
		return nil, err
	}
	if created != nil {
		return Created{*created}, nil
	}
	if err := d.output(params.Name, tool, result); err != nil {
		return nil, err
	}
	return Answered{result}, nil
}

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#task-polling
func (d *Dependency) GetTask(ctx context.Context, id TaskID) (DetailedTask, error) {
	answer, err := requestTask(ctx, d.Name, methodGetTask, &taskParams{TaskID: id})
	if err != nil {
		return nil, err
	}
	return answer.detailed()
}

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#task-update-requests
func (d *Dependency) UpdateTask(ctx context.Context, id TaskID, responses mcp.InputResponseMap) error {
	_, err := requestTask(ctx, d.Name, methodUpdateTask, &taskParams{TaskID: id, InputResponses: responses})
	return err
}

// https://tasks.extensions.modelcontextprotocol.io/specification/2026-07-28/tasks#task-cancellation
func (d *Dependency) CancelTask(ctx context.Context, id TaskID) error {
	_, err := requestTask(ctx, d.Name, methodCancelTask, &taskParams{TaskID: id})
	return err
}

func tasks(c *connecting) { c.extensions = map[string]any{tasksExtension: struct{}{}} }

type taskParams struct {
	mcp.ParamsBase
	TaskID         TaskID               `json:"taskId"`
	InputResponses mcp.InputResponseMap `json:"inputResponses,omitempty"`
}

type taskAnswer struct {
	mcp.ResultBase
	Task
	InputRequests mcp.InputRequestMap `json:"inputRequests"`
	Result        *mcp.CallToolResult `json:"result"`
	Error         *jsonrpc.Error      `json:"error"`
}

func addTaskMethods(client *mcp.Client) {
	for _, method := range []string{methodGetTask, methodUpdateTask, methodCancelTask} {
		_ = mcp.AddSendingCustomMethod[*taskParams, *taskAnswer](client, method)
	}
}

func requestTask(ctx context.Context, app, method string, params *taskParams) (*taskAnswer, error) {
	client, err := Connect(ctx, app, tasks, WithTransport(func(next http.RoundTripper) http.RoundTripper {
		return naming{next, params.TaskID}
	}))
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return mcp.CallCustomMethod[*taskParams, *taskAnswer](ctx, client, method, params)
}

func (a *taskAnswer) detailed() (DetailedTask, error) {
	switch a.Status {
	case TaskWorking:
		return WorkingTask{a.Task}, nil
	case TaskInputRequired:
		return InputRequiredTask{a.Task, a.InputRequests}, nil
	case TaskCompleted:
		return CompletedTask{a.Task, a.Result}, nil
	case TaskFailed:
		return FailedTask{a.Task, a.Error}, nil
	case TaskCancelled:
		return CancelledTask{a.Task}, nil
	}
	return nil, fmt.Errorf("task %s has unknown status %q", a.ID, a.Status)
}

type naming struct {
	next http.RoundTripper
	id   TaskID
}

func (n naming) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(nameHeader, string(n.id))
	return n.next.RoundTrip(r)
}

type creating struct {
	next    http.RoundTripper
	created **Task
}

func (c creating) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := c.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type")); mediaType == "text/event-stream" {
		response.Body = &eventReader{ReadCloser: response.Body, created: c.created}
		return response, nil
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	observe(body, c.created)
	return response, nil
}

type eventReader struct {
	io.ReadCloser
	line    []byte
	data    []byte
	created **Task
}

func (r *eventReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	unread := p[:n]
	for {
		line, rest, found := bytes.Cut(unread, []byte("\n"))
		r.line = append(r.line, line...)
		if !found {
			return n, err
		}
		r.field(bytes.TrimSuffix(r.line, []byte("\r")))
		r.line = r.line[:0]
		unread = rest
	}
}

// https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation
func (r *eventReader) field(line []byte) {
	if len(line) == 0 {
		observe(r.data, r.created)
		r.data = nil
	} else if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
		r.data = append(append(r.data, bytes.TrimPrefix(value, []byte(" "))...), '\n')
	}
}

func observe(message []byte, created **Task) {
	var response struct {
		Result struct {
			ResultType string `json:"resultType"`
			Task
		} `json:"result"`
	}
	if json.Unmarshal(message, &response) == nil && response.Result.ResultType == resultTypeTask {
		*created = &response.Result.Task
	}
}
