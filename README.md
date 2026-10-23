# Holon Go SDK

Use the official MCP Go SDK to register typed tools and pass the server to
`holon.Serve(ctx, server)`. Run the executable under Holon's host; `HOLON_HOST_URL`
is supplied by the platform. The SDK binds only a private loopback listener.

Inside a tool handler, `holon.Context(ctx).Principal` contains the admitted
issuer, subject, client ID, scopes, claims, and actor, or is nil for anonymous
requests. Platform credentials and headers are not exposed to tool handlers.

`holon.Connect(ctx, "orders")` connects an official MCP client session to a
manifest-declared dependency. Close the session after use. Calls require an
active incoming request and stop when that request ends, even when an application
retains a context without cancellation. Connections cannot be reused across
incoming requests. The broker remains responsible for authorization.
