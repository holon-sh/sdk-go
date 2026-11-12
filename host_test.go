package holon_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	holon "github.com/holon-sh/sdk-go"
)

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

// token builds a context as Holon's waypoint passes it on, with a signature nothing checks.
func token(claims string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("{}")) + "." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
}

// serve runs Serve on a free port and returns its origin once it accepts connections.
func serve(ctx context.Context, t *testing.T, server *mcp.Server, opts ...holon.Option) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	t.Setenv("PORT", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	done := make(chan error, 1)
	go func() { done <- holon.Serve(ctx, server, opts...) }()
	for !listening(address) {
		select {
		case err := <-done:
			t.Fatal(err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return "http://" + address, done
}

func listening(address string) bool {
	conn, err := net.Dial("tcp", strings.TrimPrefix(address, "http://"))
	if err == nil {
		conn.Close()
	}
	return err == nil
}

// forging sends headers only Holon's waypoint may set, and counts the responses still open.
type forging struct {
	header http.Header
	open   *sync.WaitGroup
}

func (f forging) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for name, values := range f.header {
		r.Header[name] = values
	}
	response, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		f.open.Add(1)
		response.Body = &closing{ReadCloser: response.Body, close: sync.OnceFunc(f.open.Done)}
	}
	return response, err
}

type closing struct {
	io.ReadCloser

	close func()
}

func (b *closing) Close() error {
	defer b.close()
	return b.ReadCloser.Close()
}

// callTool calls an app's tool with forged headers, and returns once its request has ended: a server ends a response only
// after the request's context is done.
func callTool(t *testing.T, endpoint string, header http.Header, name string) *mcp.CallToolResult {
	t.Helper()
	var open sync.WaitGroup
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: forging{header, &open}}, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
	client.Close()
	open.Wait()
	if err != nil || result.IsError {
		data, _ := json.Marshal(result)
		t.Fatalf("%s: %v %s", name, err, data)
	}
	return result
}

type credentials struct {
	Context       string `json:"context"`
	Authorization string `json:"authorization"`
}

var reader = &holon.Dependency{Name: "peer", Tools: map[string]holon.ToolSchema{"read": {Input: `{"type":"object"}`}}}

// startPeer binds the dependency peer, whose read tool returns the credentials it was called with.
func startPeer(t *testing.T) *mcp.Server {
	peer := mcp.NewServer(&mcp.Implementation{Name: "peer", Version: "1"}, nil)
	mcp.AddTool(peer, &mcp.Tool{Name: "read"}, func(_ context.Context, r *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, credentials, error) {
		return nil, credentials{r.Extra.Header.Get("X-Holon-Context"), r.Extra.Header.Get("Authorization")}, nil
	})
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return peer },
		&mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(server.Close)
	t.Setenv("PEER_MCP_URL", server.URL+"/mcp")
	return peer
}

type inspection struct {
	Context string      `json:"context"`
	Leaked  []string    `json:"leaked"`
	Peer    credentials `json:"peer"`
}

func TestRequestContext(t *testing.T) {
	startPeer(t)
	t.Setenv("PEER_MCP_TOKEN", "peer-secret")
	retained := make(chan context.Context, 1)
	app := mcp.NewServer(&mcp.Implementation{Name: "app", Version: "1"}, nil)
	mcp.AddTool(app, &mcp.Tool{Name: "inspect"}, func(ctx context.Context, r *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, inspection, error) {
		var leaked []string
		for name := range r.Extra.Header {
			if strings.EqualFold(name, "Authorization") || strings.HasPrefix(strings.ToLower(name), "x-holon-") {
				leaked = append(leaked, name)
			}
		}
		identity, _ := json.Marshal(holon.Context(ctx))
		peer, err := holon.Call[credentials](ctx, reader, "read", struct{}{})
		// A case that failed before taking its context must not block the next.
		select {
		case retained <- context.WithoutCancel(ctx):
		default:
		}
		return nil, inspection{Context: string(identity), Leaked: leaked, Peer: peer}, err
	})
	const (
		reporter = "spiffe://cluster.local/ns/session/sa/reporter"
		job      = "spiffe://cluster.local/ns/session/sa/install-accounts"
	)
	person := `{"exp":4102444800,"aud":["https://holon.example"],"principal":{"iss":"issuer","sub":"user","client_id":"client",` +
		`"scope":"read write","claims":{"email":"ada@example.com","custom":{"values":[1,null]}},"act":{"sub":"agent"}}}`
	user := `{"issuer":"issuer","subject":"user","clientId":"client","scopes":["read","write"],` +
		`"claims":{"custom":{"values":[1,null]},"email":"ada@example.com"},"actor":{"sub":"agent"}}`
	reporting := `{"app":"reporter","session":"session"}`
	anonymous := `{"principal":null,"caller":null}`
	subject := strings.Repeat("u", 9000)
	// The largest context Holon passes on.
	large := token(strings.Replace(person, `"sub":"user"`, `"sub":"`+subject+`"`, 1))
	large += strings.Repeat("s", 16<<10-len(large))
	for _, tc := range []struct {
		name, platform, path, context, caller, identity string
	}{
		{"signed-in person", "1", "/mcp", token(person), "", `{"principal":` + user + `,"caller":null}`},
		{"app for a signed-in person", "1", "/mcp/app", token(person), reporter, `{"principal":` + user + `,"caller":` + reporting + `}`},
		{"signed-out person", "1", "/mcp", token(`{"exp":4102444800,"aud":"https://holon.example"}`), "", anonymous},
		{"app outside a request", "1", "/mcp", "", reporter, `{"principal":null,"caller":` + reporting + `}`},
		{"lifecycle hook job", "1", "/mcp", token(`{"exp":4102444800,"aud":"https://holon.example","hook":{"app":"accounts","job":"install-accounts"}}`),
			job, `{"principal":null,"caller":{"app":"accounts","session":"session"}}`},
		{"caller outside the session's mesh", "1", "/mcp", "", "spiffe://holon.example/sa/reporter", anonymous},
		{"16 KiB context", "1", "/mcp", large, "", `{"principal":` + strings.Replace(user, `"subject":"user"`, `"subject":"`+subject+`"`, 1) + `,"caller":null}`},
		{"outside Holon", "", "/mcp", token(person), reporter, anonymous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOLON_PLATFORM", tc.platform)
			origin, _ := serve(t.Context(), t, app)
			header := http.Header{"Authorization": {"Bearer forged"}, "X-Holon-Scope": {"forged"}}
			if tc.context != "" {
				header.Set("X-Holon-Context", tc.context)
			}
			if tc.caller != "" {
				header.Set("X-Holon-Caller", tc.caller)
			}
			var got inspection
			data, _ := json.Marshal(callTool(t, origin+tc.path, header, "inspect").StructuredContent)
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Context != tc.identity || len(got.Leaked) != 0 {
				t.Fatalf("request: %.300s, leaked %v", got.Context, got.Leaked)
			}
			forwarded := ""
			if tc.platform == "1" {
				forwarded = tc.context
			}
			if got.Peer != (credentials{forwarded, "Bearer peer-secret"}) {
				t.Fatalf("peer call during the request: context %t, authorization %q", got.Peer.Context != "", got.Peer.Authorization)
			}
			late, err := holon.Call[credentials](receive(t, retained), reader, "read", struct{}{})
			if err != nil || late != (credentials{"", "Bearer peer-secret"}) {
				t.Fatalf("peer call after the request: context %t, authorization %q, %v", late.Context != "", late.Authorization, err)
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		t.Setenv("HOLON_PLATFORM", "1")
		origin, _ := serve(t.Context(), t, app)
		for name, header := range map[string]http.Header{
			"two contexts":               {"X-Holon-Context": {token(person), token(person)}},
			"two callers":                {"X-Holon-Caller": {reporter, reporter}},
			"malformed context":          {"X-Holon-Context": {"header.claims"}},
			"non-object actor":           {"X-Holon-Context": {token(strings.Replace(person, `{"sub":"agent"}`, `[{"sub":"agent"}]`, 1))}},
			"principal without a client": {"X-Holon-Context": {token(strings.Replace(person, `"client_id":"client"`, `"client_id":""`, 1))}},
		} {
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/mcp", strings.NewReader(`{}`))
			request.Header = header
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Errorf("%s: got %d", name, response.StatusCode)
			}
		}
	})
}

func TestPeerCalls(t *testing.T) {
	peer := startPeer(t)
	app := mcp.NewServer(&mcp.Implementation{Name: "app", Version: "1"}, nil)
	addDependencyChecks(peer, app)
	origin, _ := serve(t.Context(), t, app)
	callTool(t, origin+"/mcp", nil, "contracts")
	for secret, want := range map[string]string{"": "", "secret": "Bearer secret"} {
		t.Setenv("PEER_MCP_TOKEN", secret)
		if got, err := holon.Call[credentials](t.Context(), reader, "read", struct{}{}); err != nil || got != (credentials{"", want}) {
			t.Errorf("call outside a request with token %q: %+v, %v", secret, got, err)
		}
	}
	if _, err := holon.Connect(t.Context(), "missing"); err == nil || !strings.Contains(err.Error(), "set MISSING_MCP_URL") {
		t.Errorf("unbound dependency: %v", err)
	}
	var followed atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	defer elsewhere.Close()
	moved := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/mcp", http.StatusTemporaryRedirect))
	defer moved.Close()
	t.Setenv("MOVED_MCP_URL", moved.URL+"/mcp")
	if _, err := holon.Connect(t.Context(), "moved"); err == nil || followed.Load() {
		t.Errorf("redirect followed: %v", err)
	}
}

func TestFrontend(t *testing.T) {
	t.Chdir(t.TempDir())
	origin, _ := serve(t.Context(), t, mcp.NewServer(&mcp.Implementation{Name: "app", Version: "1"}, nil), holon.WithFrontend("web"))
	fetch := func(method, path string) (*http.Response, string) {
		t.Helper()
		request, _ := http.NewRequestWithContext(t.Context(), method, origin+path, nil)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response, string(body)
	}
	// The frontend may be built after the application starts.
	if response, _ := fetch(http.MethodGet, "/"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unbuilt frontend: %d", response.StatusCode)
	}
	for name, content := range map[string]string{
		"web/index.html": "<h1>Page</h1>", "web/style.css": "h1 {}", "web/LICENSE": "Licence",
		"web/docs/index.html": "<h1>Docs</h1>", "web/empty/.keep": "", "web/.private": "Hidden", "secret": "Outside",
	} {
		if err := errors.Join(os.MkdirAll(filepath.Dir(name), 0o755), os.WriteFile(name, []byte(content), 0o644)); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(os.Symlink("../secret", "web/escape"), os.Symlink("style.css", "web/alias.css")); err != nil {
		t.Fatal(err)
	}
	html, css := "text/html; charset=utf-8", "text/css; charset=utf-8"
	for path, want := range map[string]struct{ contentType, body string }{
		"/":                       {html, "<h1>Page</h1>"},
		"/index.html":             {html, "<h1>Page</h1>"},
		"/style.css":              {css, "h1 {}"},
		"/alias.css":              {css, "h1 {}"},
		"/LICENSE":                {"application/octet-stream", "Licence"},
		"/docs/":                  {html, "<h1>Docs</h1>"},
		"/docs":                   {html, "<h1>Docs</h1>"},
		"/.private":               {},
		"/%2eprivate":             {},
		"/docs/%2E%2E/index.html": {},
		"/%2findex.html":          {},
		"/escape":                 {},
		"/empty/":                 {},
		"/missing":                {},
		"/mcp/app/page":           {},
	} {
		response, body := fetch(http.MethodGet, path)
		status := http.StatusOK
		if want.body == "" {
			status = http.StatusNotFound
		}
		if response.StatusCode != status || status == http.StatusOK && (body != want.body ||
			response.Header.Get("Content-Type") != want.contentType || response.Header.Get("X-Content-Type-Options") != "nosniff") {
			t.Errorf("%s: %d %v %q", path, response.StatusCode, response.Header, body)
		}
	}
	if response, body := fetch(http.MethodHead, "/"); response.StatusCode != http.StatusOK || body != "" || response.Header.Get("Content-Type") != html {
		t.Errorf("HEAD: %d %v", response.StatusCode, response.Header)
	}
	if response, _ := fetch(http.MethodPost, "/"); response.StatusCode != http.StatusNotFound {
		t.Errorf("POST: %d", response.StatusCode)
	}
}

func TestShutdown(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	app := mcp.NewServer(&mcp.Implementation{Name: "app", Version: "1"}, nil)
	mcp.AddTool(app, &mcp.Tool{Name: "wait"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		started <- struct{}{}
		<-release
		return nil, struct{}{}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	origin, done := serve(ctx, t, app)
	transport := &http.Transport{}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: origin + "/mcp", HTTPClient: &http.Client{Transport: transport}, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	called := make(chan error, 1)
	go func() {
		_, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "wait"})
		called <- err
	}()
	receive(t, started)
	process, _ := os.FindProcess(os.Getpid())
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); listening(origin); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still listening after SIGTERM")
		}
	}
	close(release)
	if err := receive(t, called); err != nil {
		t.Errorf("call in flight: %v", err)
	}
	// Shutdown waits five seconds on a connection that never sent a request, as one the client dialed spare may be.
	transport.CloseIdleConnections()
	if err := receive(t, done); err != nil {
		t.Errorf("SIGTERM: %v", err)
	}
	origin, done = serve(ctx, t, app)
	cancel()
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("ending ctx: %v", err)
	}
	if listening(origin) {
		t.Error("listener survived shutdown")
	}
}
