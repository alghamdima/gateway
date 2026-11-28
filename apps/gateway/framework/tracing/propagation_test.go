package tracing

import (
	"testing"

	"github.com/valyala/fasthttp"
)

func TestParseTraceparent_ValidHeader(t *testing.T) {
	// Examples from the W3C spec and a real upstream service's headers
	tests := []struct {
		name        string
		traceparent string
		wantTraceID string
		wantParent  string
		wantFlags   string
	}{
		{
			name:        "valid traceparent from an upstream service",
			traceparent: "00-69538b980000000079943934f90c1d40-aad09d1659b4c7e3-01",
			wantTraceID: "69538b980000000079943934f90c1d40",
			wantParent:  "aad09d1659b4c7e3",
			wantFlags:   "01",
		},
		{
			name:        "valid traceparent with sampled flag",
			traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
			wantTraceID: "0af7651916cd43dd8448eb211c80319c",
			wantParent:  "b7ad6b7169203331",
			wantFlags:   "01",
		},
		{
			name:        "valid traceparent not sampled",
			traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00",
			wantTraceID: "0af7651916cd43dd8448eb211c80319c",
			wantParent:  "b7ad6b7169203331",
			wantFlags:   "00",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ParseTraceparent(tt.traceparent)
			if ctx == nil {
				t.Fatalf("ParseTraceparent() returned nil for valid header")
			}
			if ctx.TraceID != tt.wantTraceID {
				t.Errorf("TraceID = %q, want %q", ctx.TraceID, tt.wantTraceID)
			}
			if ctx.ParentID != tt.wantParent {
				t.Errorf("ParentID = %q, want %q", ctx.ParentID, tt.wantParent)
			}
			if ctx.TraceFlags != tt.wantFlags {
				t.Errorf("TraceFlags = %q, want %q", ctx.TraceFlags, tt.wantFlags)
			}
		})
	}
}

func TestParseTraceparent_InvalidVersion(t *testing.T) {
	// Only version 00 is supported
	tests := []struct {
		name        string
		traceparent string
	}{
		{
			name:        "version 01",
			traceparent: "01-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		},
		{
			name:        "version ff",
			traceparent: "ff-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ParseTraceparent(tt.traceparent)
			if ctx != nil {
				t.Errorf("ParseTraceparent() should return nil for unsupported version")
			}
		})
	}
}

func TestParseTraceparent_InvalidTraceID(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
	}{
		{
			name:        "trace ID too short",
			traceparent: "00-0af7651916cd43dd8448eb211c8031-b7ad6b7169203331-01",
		},
		{
			name:        "trace ID too long",
			traceparent: "00-0af7651916cd43dd8448eb211c80319c00-b7ad6b7169203331-01",
		},
		{
			name:        "trace ID with invalid chars",
			traceparent: "00-0af7651916cd43dd8448eb211c80319z-b7ad6b7169203331-01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ParseTraceparent(tt.traceparent)
			if ctx != nil {
				t.Errorf("ParseTraceparent() should return nil for invalid trace ID")
			}
		})
	}
}

func TestParseTraceparent_InvalidParentID(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
	}{
		{
			name:        "parent ID too short",
			traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b71692033-01",
		},
		{
			name:        "parent ID too long",
			traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b716920333100-01",
		},
		{
			name:        "parent ID with invalid chars",
			traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b716920333z-01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ParseTraceparent(tt.traceparent)
			if ctx != nil {
				t.Errorf("ParseTraceparent() should return nil for invalid parent ID")
			}
		})
	}
}

func TestParseTraceparent_MalformedHeader(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
	}{
		{
			name:        "empty string",
			traceparent: "",
		},
		{
			name:        "missing parts",
			traceparent: "00-0af7651916cd43dd8448eb211c80319c",
		},
		{
			name:        "too many parts",
			traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-extra",
		},
		{
			name:        "wrong delimiter",
			traceparent: "00_0af7651916cd43dd8448eb211c80319c_b7ad6b7169203331_01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ParseTraceparent(tt.traceparent)
			if ctx != nil {
				t.Errorf("ParseTraceparent() should return nil for malformed header")
			}
		})
	}
}

func TestExtractParentID_ReturnsTraceID(t *testing.T) {
	header := &fasthttp.RequestHeader{}
	header.Set(TraceParentHeader, "00-69538b980000000079943934f90c1d40-aad09d1659b4c7e3-01")

	traceID := ExtractParentID(header)
	if traceID != "69538b980000000079943934f90c1d40" {
		t.Errorf("ExtractParentID() = %q, want %q", traceID, "69538b980000000079943934f90c1d40")
	}
}

func TestExtractParentID_EmptyHeader(t *testing.T) {
	header := &fasthttp.RequestHeader{}

	traceID := ExtractParentID(header)
	if traceID != "" {
		t.Errorf("ExtractParentID() = %q, want empty string", traceID)
	}
}

func TestExtractTraceParentSpanID_ReturnsParentSpanID(t *testing.T) {
	header := &fasthttp.RequestHeader{}
	header.Set(TraceParentHeader, "00-69538b980000000079943934f90c1d40-aad09d1659b4c7e3-01")

	parentSpanID := ExtractTraceParentSpanID(header)
	if parentSpanID != "aad09d1659b4c7e3" {
		t.Errorf("ExtractTraceParentSpanID() = %q, want %q", parentSpanID, "aad09d1659b4c7e3")
	}
}

func TestExtractTraceParentSpanID_EmptyHeader(t *testing.T) {
	header := &fasthttp.RequestHeader{}

	parentSpanID := ExtractTraceParentSpanID(header)
	if parentSpanID != "" {
		t.Errorf("ExtractTraceParentSpanID() = %q, want empty string", parentSpanID)
	}
}

func TestExtractTraceParentSpanID_InvalidHeader(t *testing.T) {
	header := &fasthttp.RequestHeader{}
	header.Set(TraceParentHeader, "invalid-header")

	parentSpanID := ExtractTraceParentSpanID(header)
	if parentSpanID != "" {
		t.Errorf("ExtractTraceParentSpanID() = %q, want empty string for invalid header", parentSpanID)
	}
}

// Vendor trace state rides tracestate, not traceparent, so a connector rebuilding the
// caller's parent context has to read it separately.
func TestExtractTraceState(t *testing.T) {
	header := &fasthttp.RequestHeader{}
	if got := ExtractTraceState(header); got != "" {
		t.Errorf("ExtractTraceState() with no header = %q, want empty", got)
	}

	const traceState = "dd=s:2;t.llmobs_ml_app:checkout-agent,other=vendor"
	header.Set(TraceStateHeader, traceState)
	if got := ExtractTraceState(header); got != traceState {
		t.Errorf("ExtractTraceState() = %q, want %q", got, traceState)
	}
}
