package holon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	holon "github.com/holon-sh/sdk-go"
)

type scopeTransport struct{ scope string }

func (t scopeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Holon-Scope", t.scope)
	r.Header.Set("X-Holon-Context", "must-not-reach-app")
	r.Header.Set("Authorization", "Bearer must-not-reach-app")
	return http.DefaultTransport.RoundTrip(r)
}

type inspection struct {
	Subject string `json:"subject"`
	Peer    string `json:"peer"`
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
		var zero T
		return zero
	}
}

func TestHostedRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan string, 1)
	retained := make(chan context.Context, 2)
	peerStarted := make(chan struct{}, 2)
	peerRelease := make(chan struct{}, 2)
	peerCancelled := make(chan error, 1)
	var authorization atomic.Pointer[string]
	peer := mcp.NewServer(&mcp.Implementation{Name: "peer", Version: "1"}, nil)
	mcp.AddTool(peer, &mcp.Tool{Name: "read"}, func(ctx context.Context, r *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, inspection, error) {
		if r.Extra.Header.Get("Authorization") != "" || r.Extra.Header.Get("X-Holon-Context") != "" {
			return nil, inspection{}, errors.New("credentials leaked to peer")
		}
		peerStarted <- struct{}{}
		select {
		case <-ctx.Done():
			peerCancelled <- ctx.Err()
			return nil, inspection{}, ctx.Err()
		case <-peerRelease:
		}
		return nil, inspection{Peer: r.Extra.Header.Get("X-Holon-Scope")}, nil
	})
	peerHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer },
		&mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true})
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/broker/ready":
			var registration struct {
				Port int `json:"port"`
			}
			if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
				t.Error(err)
				return
			}
			ready <- fmt.Sprintf("http://127.0.0.1:%d/mcp", registration.Port)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/broker/context/"):
			id := strings.TrimPrefix(r.URL.Path, "/broker/context/")
			switch id {
			case "unknown":
				http.Error(w, "unknown", http.StatusForbidden)
			case "malformed":
				fmt.Fprint(w, `{"extra":`)
			case "anonymous":
				fmt.Fprint(w, `null`)
			case "expired":
				fmt.Fprint(w, `{"expiresAt":1}`)
			default:
				json.NewEncoder(w).Encode(map[string]any{
					"clientId": "browser", "scopes": []string{"read"}, "expiresAt": time.Now().Add(time.Hour).Unix(),
					"extra": map[string]any{"iss": "https://identity.example", "sub": id},
				})
			}
		case r.URL.Path == "/broker/peer/peer":
			header := r.Header.Get("Authorization")
			authorization.Store(&header)
			peerHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer broker.Close()
	defer broker.CloseClientConnections()
	defer cancel()
	t.Setenv("HOLON_HOST_URL", broker.URL+"/broker")
	t.Setenv("PEER_MCP_URL", broker.URL+"/broker/peer/peer")
	t.Setenv("ESCAPE_MCP_URL", broker.URL+"/untrusted")
	app := mcp.NewServer(&mcp.Implementation{Name: "app", Version: "1"}, nil)
	mcp.AddTool(app, &mcp.Tool{Name: "inspect"}, func(ctx context.Context, r *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, inspection, error) {
		if r.Extra.Header.Get("Authorization") != "" || r.Extra.Header.Get("X-Holon-Scope") != "" || r.Extra.Header.Get("X-Holon-Context") != "" {
			return nil, inspection{}, errors.New("headers leaked")
		}
		if _, err := holon.Connect(ctx, "escape"); err == nil || !strings.Contains(err.Error(), "not bound to the Holon host") {
			return nil, inspection{}, fmt.Errorf("unbound dependency: %w", err)
		}
		principal := holon.Context(ctx).Principal
		if principal == nil {
			return nil, inspection{Subject: "anonymous"}, nil
		}
		if principal.Issuer != "https://identity.example" || principal.ClientID != "browser" {
			return nil, inspection{}, errors.New("identity lost")
		}
		client, err := holon.Connect(ctx, "peer")
		if err != nil {
			return nil, inspection{}, err
		}
		defer client.Close()
		result, err := client.CallTool(context.WithoutCancel(ctx), &mcp.CallToolParams{Name: "read"})
		if err != nil {
			return nil, inspection{}, err
		}
		if result.IsError {
			return nil, inspection{}, errors.New("peer tool failed")
		}
		retained <- context.WithoutCancel(ctx)
		return nil, inspection{Subject: principal.Subject, Peer: result.StructuredContent.(map[string]any)["peer"].(string)}, nil
	})
	addDependencyChecks(peer, app)
	done := make(chan error, 1)
	go func() { done <- holon.Serve(ctx, app) }()
	endpoint := receive(t, ready)
	connect := func(scope string) *mcp.ClientSession {
		t.Helper()
		client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: endpoint, HTTPClient: &http.Client{Transport: scopeTransport{scope}}, DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		return client
	}
	clients := map[string]*mcp.ClientSession{"alice": connect("alice"), "bob": connect("bob"), "anonymous": connect("anonymous")}
	var wg sync.WaitGroup
	for name, client := range clients {
		wg.Go(func() {
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "inspect"})
			if err != nil {
				t.Error(err)
				return
			}
			if result.IsError {
				t.Errorf("tool error: %+v", result)
				return
			}
			got := result.StructuredContent.(map[string]any)
			if got["subject"] != name || (name != "anonymous" && got["peer"] != name) {
				t.Errorf("mixed request identity: %v", got)
			}
		})
	}
	for range 2 {
		receive(t, peerStarted)
	}
	peerRelease <- struct{}{}
	peerRelease <- struct{}{}
	wg.Wait()
	checks := connect("alice")
	result, err := checks.CallTool(ctx, &mcp.CallToolParams{Name: "contracts", Arguments: map[string]any{}})
	checks.Close()
	if err != nil || result.IsError {
		data, _ := json.Marshal(result)
		t.Fatalf("dependency contracts: %v %s", err, data)
	}
	for range 2 {
		if _, err := holon.Connect(receive(t, retained), "peer"); err == nil || !strings.Contains(err.Error(), "outside its active Holon scope") {
			t.Errorf("retained authority: %v", err)
		}
	}
	for _, scope := range []string{"", "unknown", "malformed", "expired"} {
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{}`))
		request.Header.Set("X-Holon-Scope", scope)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("%q: got %d", scope, response.StatusCode)
		}
	}
	waiting := connect("alice")
	callCtx, stop := context.WithCancel(ctx)
	defer stop()
	callDone := make(chan error, 1)
	go func() {
		_, err := waiting.CallTool(callCtx, &mcp.CallToolParams{Name: "inspect"})
		callDone <- err
	}()
	receive(t, peerStarted)
	stop()
	if err := receive(t, callDone); !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation: %v", err)
	}
	if err := receive(t, peerCancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("peer cancellation: %v", err)
	}
	cancel()
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Error(err)
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(`{}`))
	if response, err := http.DefaultClient.Do(request); err == nil {
		response.Body.Close()
		t.Error("listener survived shutdown")
	}
	dependency := &holon.Dependency{Name: "peer", Tools: map[string]holon.ToolSchema{"contract": {Input: `{"type":"object"}`}}}
	for token, want := range map[string]string{"": "", "secret": "Bearer secret"} {
		t.Setenv("PEER_MCP_TOKEN", token)
		result, err := dependency.CallTool(t.Context(), &mcp.CallToolParams{Name: "contract", Arguments: map[string]any{"id": "standalone"}})
		if err != nil || result.IsError || *authorization.Load() != want {
			t.Errorf("standalone call with token %q: %+v, %v", token, result, err)
		}
	}
}
