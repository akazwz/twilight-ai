package utils

import (
	"context"
	"net/http"
)

type requestHeadersKey struct{}

// MergeHeaders returns a fresh map with canonical HTTP header names. Later
// layers override earlier layers without retaining any caller-owned maps.
func MergeHeaders(layers ...map[string]string) map[string]string {
	headers := make(map[string]string)
	for _, layer := range layers {
		for key, value := range layer {
			headers[http.CanonicalHeaderKey(key)] = value
		}
	}
	return headers
}

// WithRequestHeaders snapshots headers onto ctx, merging any inherited values.
func WithRequestHeaders(ctx context.Context, headers map[string]string) context.Context {
	return context.WithValue(ctx, requestHeadersKey{}, MergeHeaders(RequestHeaders(ctx, nil, nil), headers))
}

// RequestHeaders merges defaults, provider headers, then context headers.
func RequestHeaders(ctx context.Context, defaults, provider map[string]string) map[string]string {
	headers, _ := ctx.Value(requestHeadersKey{}).(map[string]string)
	return MergeHeaders(defaults, provider, headers)
}
