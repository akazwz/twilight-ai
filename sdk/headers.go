package sdk

import (
	"context"

	"github.com/felinics/twilight/internal/utils"
)

// WithRequestHeaders returns a context carrying HTTP headers for provider
// requests. It copies the supplied map and merges it with inherited headers;
// the new values override inherited values, ignoring header name casing.
// Request headers override provider-level WithHeaders and default headers.
// Transport-required headers (SSE and multipart) and signing are applied last.
//
// Use a separate context per conversation for session identifiers. The context
// can be passed to generation, streaming, model listing and probes, and is
// preserved through the SDK's tool execution loop.
//
// Supported by Anthropic Messages, all OpenAI providers, Google Generative AI,
// GitHub Copilot and OpenCode Go. Other providers may ignore these headers.
func WithRequestHeaders(ctx context.Context, headers map[string]string) context.Context {
	return utils.WithRequestHeaders(ctx, headers)
}
