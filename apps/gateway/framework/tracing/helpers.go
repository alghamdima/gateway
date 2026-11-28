// Package tracing provides distributed tracing infrastructure for Bifrost
package tracing

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
)

// GetTraceID retrieves the trace ID from the context
func GetTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	traceID, ok := ctx.Value(schemas.BifrostContextKeyTraceID).(string)
	if !ok {
		return ""
	}
	return traceID
}
