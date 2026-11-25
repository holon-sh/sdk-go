package holon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Environment proxy settings would send a peer's cluster-local address elsewhere.
var direct = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

var ErrNotBound = errors.New("not bound")

type ConnectOption func(*connecting)

type connecting struct {
	wraps []func(http.RoundTripper) http.RoundTripper
}

func WithTransport(wrap func(http.RoundTripper) http.RoundTripper) ConnectOption {
	return func(c *connecting) { c.wraps = append(c.wraps, wrap) }
}

func Connect(ctx context.Context, name string, opts ...ConnectOption) (*mcp.ClientSession, error) {
	var options connecting
	for _, opt := range opts {
		opt(&options)
	}
	variable := protocol.DependencyEnvName(name)
	endpoint := os.Getenv(variable)
	if endpoint == "" {
		return nil, fmt.Errorf("dependency %q is %w: set %s", name, ErrNotBound, variable)
	}
	scope, _ := ctx.Value(scopeKey{}).(*requestScope)
	as, _ := ctx.Value(actingKey{}).(*acting)
	if as != nil && as.subject == "" {
		return nil, errors.New("ActAs needs the subject of a person")
	}
	token := os.Getenv(strings.TrimSuffix(variable, "URL") + "TOKEN")
	var transport http.RoundTripper = peerTransport{scope: scope, acting: as, token: token}
	for _, wrap := range options.wraps {
		transport = wrap(transport)
	}
	httpClient := &http.Client{Transport: transport, CheckRedirect: noRedirect}
	// Every client can be asked. Servers must not assume a client answers: https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr#server-requirements-basic-workflow
	client := mcp.NewClient(&mcp.Implementation{Name: "holon-go", Version: "0.1.0"}, &mcp.ClientOptions{
		Capabilities:   &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	})
	client.AddSendingMiddleware(knownPeer)
	return client.Connect(context.WithValue(ctx, connectingKey{}, true), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClient,
		MaxRetries: -1,
	}, nil)
}

type connectingKey struct{}

// https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning#protocol-version-negotiation
func knownPeer(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method != "server/discover" || ctx.Value(connectingKey{}) == nil {
			return next(ctx, method, request)
		}
		return &mcp.DiscoverResult{SupportedVersions: []string{"2026-07-28"}}, nil
	}
}

func noRedirect(*http.Request, []*http.Request) error {
	return errors.New("MCP endpoint redirects are not allowed")
}

type acting struct{ subject, actor string }

type actingKey struct{}

type ActAsOption func(*acting)

// ActAs returns a context in which calls to other apps are made for subject, a person's principal subject, the subject
// of [RFC 8693 §2.1].
//
// [RFC 8693 §2.1]: https://www.rfc-editor.org/rfc/rfc8693.html#section-2.1
func ActAs(ctx context.Context, subject string, opts ...ActAsOption) context.Context {
	a := &acting{subject: subject}
	for _, opt := range opts {
		opt(a)
	}
	return context.WithValue(ctx, actingKey{}, a)
}

// WithActor names app, when it is not this app, as the one acting for the subject, the actor of [RFC 8693 §2.1].
//
// [RFC 8693 §2.1]: https://www.rfc-editor.org/rfc/rfc8693.html#section-2.1
func WithActor(app string) ActAsOption {
	return func(a *acting) { a.actor = app }
}

type peerTransport struct {
	scope  *requestScope
	acting *acting
	token  string
}

func (t peerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	switch {
	case t.acting != nil:
		r.Header.Set(runtimecontract.SubjectHeader, t.acting.subject)
		if t.acting.actor != "" {
			r.Header.Set(runtimecontract.ActorHeader, t.acting.actor)
		}
	case t.scope != nil && t.scope.token != "" && t.scope.ctx.Err() == nil:
		r.Header.Set(runtimecontract.ContextHeader, t.scope.token)
	}
	if t.token != "" {
		r.Header.Set(protocol.HeaderAuthorization, protocol.BearerPrefix+t.token)
	}
	return direct.RoundTrip(r)
}
