// Package tracing provides distributed tracing infrastructure for Bifrost
package tracing

import (
	"strings"

	"github.com/valyala/fasthttp"
)

// W3C Trace Context header names
const (
	TraceParentHeader = "traceparent"
	TraceStateHeader  = "tracestate"
)

// W3CTraceContext holds parsed W3C trace context values
type W3CTraceContext struct {
	TraceID    string // 32 hex characters
	ParentID   string // 16 hex characters (span ID of parent)
	TraceFlags string // 2 hex characters
	TraceState string // Optional vendor-specific trace state
}

// ExtractParentID extracts the trace ID from W3C traceparent header.
// This returns the trace ID (32 hex chars) which should be used to continue
// the distributed trace from the upstream service.
// Returns empty string if header is not present or invalid.
func ExtractParentID(header *fasthttp.RequestHeader) string {
	traceParent := string(header.Peek(TraceParentHeader))
	if traceParent == "" {
		return ""
	}
	ctx := ParseTraceparent(traceParent)
	if ctx == nil {
		return ""
	}
	return ctx.TraceID
}

// ExtractTraceParentSpanID extracts the parent span ID from W3C traceparent header.
// This returns the span ID (16 hex chars) of the upstream service's span that
// initiated this request. This should be set as the ParentID of the root span
// in the receiving service to establish the parent-child relationship.
// Returns empty string if header is not present or invalid.
func ExtractTraceParentSpanID(header *fasthttp.RequestHeader) string {
	traceParent := string(header.Peek(TraceParentHeader))
	if traceParent == "" {
		return ""
	}
	ctx := ParseTraceparent(traceParent)
	if ctx == nil {
		return ""
	}
	return ctx.ParentID
}

// ExtractTraceState returns the inbound W3C tracestate, or "" when absent.
func ExtractTraceState(header *fasthttp.RequestHeader) string {
	return string(header.Peek(TraceStateHeader))
}

// ParseTraceparent parses a W3C traceparent header value
// Format: version-traceid-parentid-traceflags
// Example: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01
func ParseTraceparent(traceparent string) *W3CTraceContext {
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 {
		return nil
	}

	version := parts[0]
	traceID := parts[1]
	parentID := parts[2]
	traceFlags := parts[3]

	// Validate version (only 00 is currently supported)
	if version != "00" {
		return nil
	}

	// Validate trace ID (32 hex characters)
	if len(traceID) != 32 || !isHex(traceID) {
		return nil
	}

	// Validate parent ID (16 hex characters)
	if len(parentID) != 16 || !isHex(parentID) {
		return nil
	}

	// Validate trace flags (2 hex characters)
	if len(traceFlags) != 2 || !isHex(traceFlags) {
		return nil
	}

	return &W3CTraceContext{
		TraceID:    traceID,
		ParentID:   parentID,
		TraceFlags: traceFlags,
	}
}

// isHex checks if a string contains only hexadecimal characters
func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
