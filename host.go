package holon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
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

type RequestContext struct {
	Principal *Principal `json:"principal"`
}

type scopeKey struct{}

type requestScope struct {
	ctx       context.Context //nolint:containedctx
	id, host  string
	identity  RequestContext
	transport *http.Transport
}

func Context(ctx context.Context) RequestContext {
	if scope, ok := ctx.Value(scopeKey{}).(*requestScope); ok {
		return scope.identity
	}
	return RequestContext{}
}

type identity struct {
	ClientID  string   `json:"clientId"`
	Scopes    []string `json:"scopes"`
	ExpiresAt float64  `json:"expiresAt"`
	Extra     struct {
		Issuer  string                     `json:"iss"`
		Subject string                     `json:"sub"`
		Claims  map[string]json.RawMessage `json:"claims"`
		Actor   json.RawMessage            `json:"act"`
	} `json:"extra"`
}

// Option configures Serve.
type Option func(*options)

type options struct{ frontend string }

// WithFrontend has the host serve dir, a directory inside the application such as a built view, as
// the application's page. A relative dir is resolved against the application's directory.
func WithFrontend(dir string) Option {
	return func(o *options) { o.frontend = dir }
}

func Serve(ctx context.Context, server *mcp.Server, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	host := os.Getenv("HOLON_HOST_URL")
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" || strings.HasSuffix(u.Path, "/") {
		return errors.New("HOLON_HOST_URL must identify the private Holon host broker")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	control := &http.Client{Transport: transport, CheckRedirect: noRedirect, Timeout: 10 * time.Second}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true})
	app := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ids := r.Header.Values("X-Holon-Scope")
			if len(ids) != 1 || ids[0] == "" {
				http.Error(w, "Request context unavailable", http.StatusForbidden)
				return
			}
			request, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, host+"/context/"+url.PathEscape(ids[0]), nil)
			response, err := control.Do(request)
			if err != nil {
				http.Error(w, "Request context unavailable", http.StatusForbidden)
				return
			}
			defer response.Body.Close()
			var admitted *identity
			if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&admitted) != nil {
				http.Error(w, "Request context unavailable", http.StatusForbidden)
				return
			}
			scope := &requestScope{ctx: r.Context(), id: ids[0], host: host, transport: transport}
			if admitted != nil {
				if admitted.ExpiresAt <= float64(time.Now().UnixNano())/1e9 {
					http.Error(w, "Request context expired", http.StatusForbidden)
					return
				}
				if admitted.Extra.Issuer != "" || admitted.Extra.Subject != "" {
					if admitted.Extra.Issuer == "" || admitted.Extra.Subject == "" || admitted.ClientID == "" {
						http.Error(w, "Malformed request identity", http.StatusForbidden)
						return
					}
					scope.identity.Principal = &Principal{
						Issuer: admitted.Extra.Issuer, Subject: admitted.Extra.Subject,
						ClientID: admitted.ClientID, Scopes: admitted.Scopes, Claims: admitted.Extra.Claims, Actor: admitted.Extra.Actor,
					}
				}
			}
			r = r.Clone(context.WithValue(r.Context(), scopeKey{}, scope))
			for name := range r.Header {
				if credentialHeader(name) {
					r.Header.Del(name)
				}
			}
			handler.ServeHTTP(w, r)
		}),
	}
	defer app.Close()
	done := make(chan error, 1)
	go func() { done <- app.Serve(listener) }()
	payload, err := json.Marshal(struct {
		Port     int    `json:"port"`
		Frontend string `json:"frontend,omitempty"`
	}{listener.Addr().(*net.TCPAddr).Port, o.frontend})
	if err != nil {
		return err
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, host+"/ready", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := control.Do(request)
	if err != nil {
		return fmt.Errorf("register application: %w", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("register application: HTTP %d", response.StatusCode)
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func credentialHeader(name string) bool {
	return strings.EqualFold(name, "Authorization") || strings.HasPrefix(strings.ToLower(name), "x-holon-")
}

func noRedirect(*http.Request, []*http.Request) error {
	return errors.New("MCP endpoint redirects are not allowed")
}
