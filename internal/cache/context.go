package cache

import "context"

type contextKey int

const (
	publicBaseKey contextKey = iota
	requestInfoKey
)

type requestInfo struct {
	id     string
	client string
}

// WithPublicBase provides the public base URL for URL rewriting.
func WithPublicBase(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, publicBaseKey, base)
}

// WithRequestInfo provides request logging fields.
func WithRequestInfo(ctx context.Context, id, client string) context.Context {
	return context.WithValue(ctx, requestInfoKey, requestInfo{id: id, client: client})
}

func publicBaseFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	value, ok := ctx.Value(publicBaseKey).(string)
	return value, ok
}

func requestInfoFromContext(ctx context.Context) requestInfo {
	if ctx == nil {
		return requestInfo{}
	}
	value, _ := ctx.Value(requestInfoKey).(requestInfo)
	return value
}
