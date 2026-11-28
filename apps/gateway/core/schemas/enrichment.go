package schemas

// EnrichmentDim describes one identity/context dimension attached to a
// request's telemetry — which team, customer, virtual key it belongs to.
//
// The Prometheus labels (plugins/telemetry) are derived from this registry and a
// conformance test pins them to it. The otel plugin projects the whole attribute
// map and needs no derivation.
type EnrichmentDim struct {
	// Name is used verbatim as the Prometheus label.
	Name string
	// SpanAttr is the span-attribute key connectors read the dimension from.
	SpanAttr string
	Tier     DimensionTier
	// Multi marks an array-valued dimension: governance can attach several teams
	// or customers to one request. Never metric-safe.
	Multi bool
}

// DimensionTier says where a dimension may be emitted.
type DimensionTier int

const (
	// tierUnset is the zero value, so a dimension that omits Tier is detectable
	// rather than silently classified. TestTierIsExplicit rejects it.
	tierUnset DimensionTier = iota
	// TierRecord never becomes a metric label.
	TierRecord
	// TierMetric is bounded and safe as a metric label everywhere.
	TierMetric
	// TierHighCardinalityMetric is unbounded, so it multiplies series rather than
	// adding a dimension. Prometheus cannot drop a label after the fact, so it
	// keeps these behind user_labels_enabled.
	TierHighCardinalityMetric
)

// MetricSafe reports whether the dimension may be a metric label anywhere.
func (d EnrichmentDim) MetricSafe() bool { return d.Tier == TierMetric }

// MetricAllowedHighCardinality reports whether a backend tolerating unbounded
// label values may emit the dimension.
func (d EnrichmentDim) MetricAllowedHighCardinality() bool {
	return d.Tier == TierMetric || d.Tier == TierHighCardinalityMetric
}

// EnrichmentDims is the ordered registry. Order is stable so derived lists are
// deterministic; adding an entry here reaches every list derived from it.
var EnrichmentDims = []EnrichmentDim{
	// --- Metric tier: bounded, safe as labels on any backend. ---
	{Name: "provider", SpanAttr: AttrBifrostProviderName, Tier: TierMetric},
	{Name: "model", SpanAttr: AttrRequestModel, Tier: TierMetric},
	{Name: "method", SpanAttr: AttrLegacyRequestType, Tier: TierMetric},
	// Derived post-response in framework/tracing, then read like any other dimension.
	{Name: "alias", SpanAttr: AttrBifrostAlias, Tier: TierMetric},
	{Name: "app", SpanAttr: AttrBifrostApp, Tier: TierMetric},
	{Name: "routing_engine_used", SpanAttr: AttrBifrostRoutingEngineUsed, Tier: TierMetric},
	{Name: "virtual_key_id", SpanAttr: AttrBifrostVirtualKeyID, Tier: TierMetric},
	{Name: "virtual_key_name", SpanAttr: AttrBifrostVirtualKeyName, Tier: TierMetric},
	{Name: "selected_key_id", SpanAttr: AttrBifrostSelectedKeyID, Tier: TierMetric},
	{Name: "selected_key_name", SpanAttr: AttrBifrostSelectedKeyName, Tier: TierMetric},
	{Name: "routing_rule_id", SpanAttr: AttrBifrostRoutingRuleID, Tier: TierMetric},
	{Name: "routing_rule_name", SpanAttr: AttrBifrostRoutingRuleName, Tier: TierMetric},
	// complexity_tier and complexity_mechanism are set by the governance plugin only
	// when a routing rule references complexity_tier. Both are closed value sets
	// (tiers: SIMPLE/MEDIUM/COMPLEX; mechanisms: semantic/llm/session/skipped), so
	// they are metric-safe. The raw complexity score is deliberately NOT a
	// dimension — unbounded cardinality; it lives only in the logstore columns.
	{Name: "complexity_tier", SpanAttr: AttrBifrostComplexityTier, Tier: TierMetric},
	{Name: "complexity_mechanism", SpanAttr: AttrBifrostComplexityMechanism, Tier: TierMetric},
	{Name: "team_id", SpanAttr: AttrBifrostTeamID, Tier: TierMetric},
	{Name: "team_name", SpanAttr: AttrBifrostTeamName, Tier: TierMetric},
	{Name: "customer_id", SpanAttr: AttrBifrostCustomerID, Tier: TierMetric},
	{Name: "customer_name", SpanAttr: AttrBifrostCustomerName, Tier: TierMetric},
	{Name: "fallback_index", SpanAttr: AttrBifrostFallbackIndex, Tier: TierMetric},

	// --- High cardinality. ---
	{Name: "user_id", SpanAttr: AttrBifrostUserID, Tier: TierHighCardinalityMetric},
	{Name: "user_name", SpanAttr: AttrBifrostUserName, Tier: TierHighCardinalityMetric},
	{Name: "user_email", SpanAttr: AttrBifrostUserEmail, Tier: TierRecord},
	{Name: "team_ids", SpanAttr: AttrBifrostTeamIDs, Multi: true, Tier: TierRecord},
	{Name: "team_names", SpanAttr: AttrBifrostTeamNames, Multi: true, Tier: TierRecord},
	{Name: "customer_ids", SpanAttr: AttrBifrostCustomerIDs, Multi: true, Tier: TierRecord},
	{Name: "customer_names", SpanAttr: AttrBifrostCustomerNames, Multi: true, Tier: TierRecord},
}

// HighCardinalityMetricEnrichmentDimNames returns the names of the metric-safe dimensions plus
// the unbounded ones, in registry order.
func HighCardinalityMetricEnrichmentDimNames() []string {
	out := make([]string, 0, len(EnrichmentDims))
	for _, d := range EnrichmentDims {
		if d.MetricAllowedHighCardinality() {
			out = append(out, d.Name)
		}
	}
	return out
}

// MetricSafeEnrichmentDimNames returns the names of the bounded dimensions, in registry order.
func MetricSafeEnrichmentDimNames() []string {
	out := make([]string, 0, len(EnrichmentDims))
	for _, d := range EnrichmentDims {
		if d.MetricSafe() {
			out = append(out, d.Name)
		}
	}
	return out
}
