package otel

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The error counter must carry both status_code and error_type: status alone cannot
// attribute fault.
func TestRecordErrorRequestCarriesStatusAndErrorType(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m := &MetricsExporter{provider: provider, meter: provider.Meter("test")}
	m.initMetrics()

	now := time.Now()
	errSpan := func(model string, attrs map[string]any) *schemas.Span {
		attrs[schemas.AttrProviderName] = "openai"
		attrs[schemas.AttrRequestModel] = model
		return &schemas.Span{
			Kind:      schemas.SpanKindLLMCall,
			StartTime: now, EndTime: now.Add(time.Second),
			Status:     schemas.SpanStatusError,
			Attributes: attrs,
		}
	}

	trace := &schemas.Trace{
		Spans: []*schemas.Span{
			// Internally-raised failure: resolves to 500, attributed to Bifrost.
			errSpan("gpt-image-1", map[string]any{
				schemas.AttrHTTPResponseStatusCode: 500,
				schemas.AttrBifrostErrorType:       string(schemas.ErrorTypeBifrostInternal),
			}),
			// Provider 500: same status, different fault.
			errSpan("gpt-4o", map[string]any{
				schemas.AttrHTTPResponseStatusCode: 500,
				schemas.AttrBifrostErrorType:       string(schemas.ErrorTypeProviderServerError),
			}),
			// Unclassified failure still gets a label rather than an empty one.
			errSpan("gpt-4o-mini", map[string]any{}),
		},
	}

	(&OtelPlugin{}).recordMetricsFromTrace(context.Background(), m, trace, false)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := make(map[string]string, 3)
	for _, sm := range rm.ScopeMetrics {
		for _, mtr := range sm.Metrics {
			if mtr.Name != "bifrost_error_requests_total" {
				continue
			}
			sum, ok := mtr.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is %T, want Sum[int64]", mtr.Name, mtr.Data)
			}
			for _, dp := range sum.DataPoints {
				model, _ := dp.Attributes.Value("model")
				status, _ := dp.Attributes.Value("status_code")
				errorType, hasErrorType := dp.Attributes.Value("error_type")
				if !hasErrorType {
					t.Errorf("model %q: error_type dimension missing", model.AsString())
				}
				got[model.AsString()] = status.AsString() + "/" + errorType.AsString()
			}
		}
	}

	want := map[string]string{
		"gpt-image-1": "500/" + string(schemas.ErrorTypeBifrostInternal),
		"gpt-4o":      "500/" + string(schemas.ErrorTypeProviderServerError),
		"gpt-4o-mini": "unknown/" + string(schemas.ErrorTypeOther),
	}
	for model, wantLabels := range want {
		if got[model] != wantLabels {
			t.Errorf("model %q: status_code/error_type = %q, want %q", model, got[model], wantLabels)
		}
	}
}

// The replica id is emitted twice on purpose, under DIFFERENT names: service.instance.id
// on the resource, for backends that read it natively, and bifrost_instance_id as a
// datapoint label, for per-replica breakdown. They must never share a name: a collector
// with resource_to_telemetry_conversion renders the resource attribute as
// service_instance_id, and a datapoint label of that name would duplicate it and make
// the Prometheus exporter drop the whole metric.
func TestReplicaIDLabelCannotCollideWithTheConvertedResourceAttribute(t *testing.T) {
	attrs := BuildBifrostAttributes("openai", "gpt-4o", "chat", "", "", "", "", 0, "", "", "", "")

	var replicaLabel string
	for _, kv := range attrs {
		if kv.Key == "service_instance_id" {
			t.Error("datapoint label is named service_instance_id, which is exactly what resource_to_telemetry_conversion produces from service.instance.id; the duplicate drops the metric")
		}
		if kv.Key == "bifrost_instance_id" {
			replicaLabel = kv.Value.Emit()
		}
	}
	if replicaLabel == "" {
		t.Error("bifrost_instance_id missing from the datapoint attributes; per-replica breakdown needs it on every exporter")
	}

	res, err := newMetricsResource("bifrost-test")
	if err != nil {
		t.Fatalf("newMetricsResource: %v", err)
	}
	var sawInstance, sawServiceName bool
	for _, kv := range res.Attributes() {
		switch kv.Key {
		case "service.instance.id":
			sawInstance = true
			if kv.Value.Emit() != replicaLabel {
				t.Errorf("resource %q = %q but label = %q; both must identify the same replica", kv.Key, kv.Value.Emit(), replicaLabel)
			}
		case "service.name":
			sawServiceName = true
		}
	}
	if !sawInstance {
		t.Error("service.instance.id missing from the resource; OTLP-native backends read instance identity from there")
	}
	if !sawServiceName {
		t.Error("service.name missing from the resource")
	}
}
