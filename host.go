package holon

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Principal struct {
	Issuer   string                     `json:"issuer"`
	Subject  string                     `json:"subject"`
	ClientID string                     `json:"clientId"`
	Scopes   []string                   `json:"scopes"`
	Claims   map[string]json.RawMessage `json:"claims"`
	Actor    json.RawMessage            `json:"actor,omitempty"`
}

// Caller is the app of the same session that made a request. A lifecycle hook's job calls as its app.
type Caller struct {
	App     string `json:"app"`
	Session string `json:"session"`
}

type RequestContext struct {
	Principal *Principal `json:"principal"`
	Caller    *Caller    `json:"caller"`
}

type scopeKey struct{}

type requestScope struct {
	ctx      context.Context //nolint:containedctx
	token    string
	identity RequestContext
}

func Context(ctx context.Context) RequestContext {
	if scope, ok := ctx.Value(scopeKey{}).(*requestScope); ok {
		return scope.identity
	}
	return RequestContext{}
}

// The forms of the headers Holon's waypoint sets: a context is a compact JWS, whose payload segment is matched, and a
// caller is the SPIFFE ID of an app of the session.
var (
	contextForm = regexp.MustCompile(`^[A-Za-z0-9_-]+\.([A-Za-z0-9_-]+)\.[A-Za-z0-9_-]+$`)
	callerForm  = regexp.MustCompile(`^spiffe://cluster\.local/ns/([^/]+)/sa/([^/]+)$`)
)

// admit reads the caller from the headers Holon's waypoint sets once it has verified the context's signature and
// expiry, and reports false when they are malformed. The waypoint names the calling app of the session, a lifecycle
// hook's job as its app.
func admit(r *http.Request) (*requestScope, bool) {
	contexts, callers := r.Header.Values("X-Holon-Context"), r.Header.Values("X-Holon-Caller")
	if len(contexts) > 1 || len(callers) > 1 {
		return nil, false
	}
	var claims *struct {
		Principal *struct {
			Issuer   string                     `json:"iss"`
			Subject  string                     `json:"sub"`
			ClientID string                     `json:"client_id"`
			Scope    string                     `json:"scope"`
			Claims   map[string]json.RawMessage `json:"claims"`
			Actor    map[string]json.RawMessage `json:"act"`
		} `json:"principal"`
	}
	scope := &requestScope{ctx: r.Context()}
	if len(contexts) == 1 {
		scope.token = contexts[0]
		form := contextForm.FindStringSubmatch(scope.token)
		if form == nil {
			return nil, false
		}
		payload, err := base64.RawURLEncoding.DecodeString(form[1])
		if err != nil || json.Unmarshal(payload, &claims) != nil || claims == nil {
			return nil, false
		}
	}
	if len(callers) == 1 {
		caller := callerForm.FindStringSubmatch(callers[0])
		if caller == nil {
			return nil, false
		}
		scope.identity.Caller = &Caller{App: caller[2], Session: caller[1]}
	}
	if claims != nil && claims.Principal != nil {
		p := claims.Principal
		scope.identity.Principal = &Principal{
			Issuer: p.Issuer, Subject: p.Subject, ClientID: p.ClientID, Claims: p.Claims,
			Scopes: strings.FieldsFunc(p.Scope, func(char rune) bool { return char == ' ' }),
		}
		if p.Actor != nil {
			scope.identity.Principal.Actor, _ = json.Marshal(p.Actor)
		}
	}
	return scope, true
}

// Option configures Serve.
type Option func(*options)

type options struct{ frontend string }

// WithFrontend serves dir, such as a built view, as the application's page: its files answer every GET and HEAD outside
// the MCP endpoint. A relative dir is resolved against the working directory.
func WithFrontend(dir string) Option {
	return func(o *options) { o.frontend = dir }
}

// Serve serves server at /mcp and /mcp/<app> on HOST and PORT, 127.0.0.1:3000 unless they are set, until ctx ends or
// the process receives SIGTERM, after which requests in flight finish.
func Serve(ctx context.Context, server *mcp.Server, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	// Inside Holon only its waypoint reaches the app, and it replaces every X-Holon- header a client sends.
	platform := os.Getenv("HOLON_PLATFORM") == "1"
	terminated, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	address := net.JoinHostPort(cmp.Or(os.Getenv("HOST"), "127.0.0.1"), cmp.Or(os.Getenv("PORT"), "3000"))
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", address)
	if err != nil {
		return err
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true})
	app := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dir, file := path.Split(r.URL.Path)
			switch {
			case r.URL.Path == "/mcp" || dir == "/mcp/" && file != "":
				scope := &requestScope{ctx: r.Context()}
				if platform {
					var ok bool
					// A context the waypoint could not have sent is a malformed request (RFC 9110 §15.5.1).
					if scope, ok = admit(r); !ok {
						http.Error(w, "Malformed Holon context", http.StatusBadRequest)
						return
					}
				}
				r = r.Clone(context.WithValue(r.Context(), scopeKey{}, scope))
				for name := range r.Header {
					if credentialHeader(name) {
						r.Header.Del(name)
					}
				}
				handler.ServeHTTP(w, r)
			case o.frontend != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
				frontend(w, r, o.frontend)
			default:
				http.NotFound(w, r)
			}
		}),
	}
	defer app.Close()
	done := make(chan error, 1)
	go func() { done <- app.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-terminated.Done():
	}
	stop()
	// After SIGTERM requests in flight finish, and when ctx ends they end with it.
	err = app.Shutdown(ctx)
	return cmp.Or(ctx.Err(), err)
}

func credentialHeader(name string) bool {
	return strings.EqualFold(name, "Authorization") || strings.HasPrefix(strings.ToLower(name), "x-holon-")
}

// frontend serves the file at the request's path in directory, or a directory's index.html. Like Holon, it serves
// nothing hidden, however its dot is encoded, and nothing a symlink leads to outside directory.
func frontend(w http.ResponseWriter, r *http.Request, directory string) {
	// Localize refuses a name the system would split differently, such as one with a backslash on Windows.
	name, err := filepath.Localize(cmp.Or(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/"), "."))
	if err != nil || strings.Contains("/"+r.URL.Path, "/.") {
		http.NotFound(w, r)
		return
	}
	// The directory may be built while the application runs, so each request opens it.
	root, err := os.OpenRoot(directory)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	info, err := root.Stat(name)
	if err == nil && info.IsDir() {
		name = filepath.Join(name, "index.html")
		info, err = root.Stat(name)
	}
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	file, err := root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	// Go's MIME lookup gives text types charset=utf-8 already.
	w.Header().Set("Content-Type", cmp.Or(mime.TypeByExtension(filepath.Ext(name)), "application/octet-stream"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), file)
}
