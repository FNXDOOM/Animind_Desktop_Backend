package auth

import "context"

// Client metadata travels through the context rather than as a parameter on every method,
// because session rows record the user agent and address that created them.

// Client describes the device presenting a credential.
type Client struct {
	UserAgent string
	IPAddress string
}

type clientInfoKey struct{}

// WithClient attaches request metadata used when creating sessions.
func WithClient(ctx context.Context, client Client) context.Context {
	return context.WithValue(ctx, clientInfoKey{}, client)
}

func clientFrom(ctx context.Context) Client {
	if c, ok := ctx.Value(clientInfoKey{}).(Client); ok {
		return c
	}
	return Client{}
}

func userAgent(ctx context.Context) string {
	c := clientFrom(ctx)
	if len(c.UserAgent) > 512 {
		return c.UserAgent[:512]
	}
	return c.UserAgent
}

func clientIP(ctx context.Context) string { return clientFrom(ctx).IPAddress }
