/**
 * A client for the parts of the gateway's governance API the control plane
 * uses: one customer per organization, one virtual key per dashboard key
 * (docs/platform/adr/0004-key-model.md).
 */

export const LIMIT_RESETS = ["daily", "weekly", "monthly"] as const;
export type LimitReset = (typeof LIMIT_RESETS)[number];

export interface GatewayClientOptions {
  url: string;
  username?: string;
  password?: string;
  /** Per request. */
  timeoutMs?: number;
  fetch?: typeof fetch;
}

export interface CreateVirtualKeyInput {
  name: string;
  customerId: string;
  expiresAt?: Date | null;
  /** Default: everything. */
  access?: VirtualKeyAccess;
}

export interface UpdateVirtualKeyInput {
  name?: string;
  isActive?: boolean;
  /**
   * A new expiry, which the gateway requires to be in the future. Only the
   * platform's maximum key lifetime shortens a key's expiry (ADMIN-12); a
   * customer can never change it (KEYS-03).
   */
  expiresAt?: Date;
}

export interface CreatedVirtualKey {
  id: string;
  /** The key itself. Returned once; never store it. */
  value: string;
}

/** The gateway answered with an error status, or could not be reached. */
export class GatewayError extends Error {
  constructor(
    message: string,
    readonly status?: number,
  ) {
    super(message);
    this.name = "GatewayError";
  }
}

/** One model the gateway can serve, from its catalog. Costs are USD per token. */
export interface GatewayModel {
  name: string;
  provider: string;
  /** With a key filter: the provider keys among them that serve the model. */
  accessible_by_keys?: string[];
  /** Unix seconds: when the provider says it created the model; absent when it gave no time. */
  created?: number;
  context_length?: number;
  max_input_tokens?: number;
  max_output_tokens?: number;
  input_cost_per_token?: number;
  output_cost_per_token?: number;
  cache_creation_input_token_cost?: number;
  cache_read_input_token_cost?: number;
  architecture?: {
    modality?: string;
    tokenizer?: string;
    instruct_type?: string;
    input_modalities?: string[];
    output_modalities?: string[];
  };
  is_deprecated?: boolean;
  /** Present when a gateway pricing override changes a cost: the cost actually charged. */
  overridden_pricing?: {
    input_cost_per_token?: number;
    output_cost_per_token?: number;
    cache_creation_input_token_cost?: number;
    cache_read_input_token_cost?: number;
  };
}

/** One request as the gateway logged it; content is never stored (ADR 0014). */
export interface GatewayLog {
  id: string;
  customer_id?: string | null;
  virtual_key_id?: string | null;
  provider?: string;
  model?: string;
  status?: string;
  stop_reason?: string | null;
  stream?: boolean;
  /** Milliseconds, request received to response finished. */
  latency?: number;
  /** Milliseconds the provider took (with its retries): the generation time. */
  upstream_latency?: number | null;
  /** Milliseconds the gateway itself took. */
  overhead_latency?: number | null;
  /** Streams only: milliseconds from the request to the first chunk. */
  time_to_first_token?: number | null;
  session_id?: string | null;
  service_tier?: string | null;
  /** The provider's usage, as the gateway normalized it; never content. */
  token_usage?: {
    prompt_tokens?: number;
    completion_tokens?: number;
    prompt_tokens_details?: {
      cached_read_tokens?: number;
      cached_write_tokens?: number;
    } | null;
    completion_tokens_details?: { reasoning_tokens?: number } | null;
  } | null;
  timestamp?: string;
}

/** One UTC day of an organization's requests, from the gateway's log. */
export interface GatewayGenerationDay {
  /** YYYY-MM-DD, UTC. */
  day: string;
  /** Client requests that reached the model step, once each however many attempts they took. */
  generations: number;
  /** Generations no attempt of which succeeded. */
  failed: number;
  /** Requests refused before any provider was called. */
  refused: number;
}

/** Milliseconds: an average and two percentiles; zero when nothing was measured. */
export interface GatewayLatencySummary {
  avg: number;
  p50: number;
  p90: number;
}

/**
 * One endpoint's attempts in one UTC hour, as the gateway counted them from
 * its log (`GET /api/logs/endpoints/hourly`). An endpoint is the provider, the
 * model, the provider key that carried the call and the service tier.
 */
export interface GatewayEndpointHour {
  /** The start of the hour, RFC 3339, UTC. */
  hour: string;
  provider: string;
  model: string;
  /** "" for a keyless provider. */
  key_id: string;
  /** "" when the provider reported none. */
  service_tier: string;
  /** Calls that reached the provider and ended. */
  attempts: number;
  succeeded: number;
  streamed: number;
  prompt_tokens: number;
  completion_tokens: number;
  reasoning_tokens: number;
  cached_read_tokens: number;
  input_cost_usd: number;
  output_cost_usd: number;
  cost_usd: number;
  /** Histograms over the answered calls: one count per bucket. */
  latency_ms: number[];
  time_to_first_token_ms: number[];
  throughput_tokens_per_second: number[];
}

/** Every endpoint's hours in a window, with the upper bounds of the histogram buckets. */
export interface GatewayEndpointStats {
  bounds: {
    latency_ms: number[];
    throughput_tokens_per_second: number[];
  };
  hours: GatewayEndpointHour[];
}

/**
 * One gateway customer's requests over a window (the gateway's
 * GET /api/logs/generations/summary, ours): generations counted once each,
 * refusals apart by reason, timings of the successful generations.
 */
export interface GatewayGenerationSummary {
  by_day: GatewayGenerationDay[];
  by_model: { provider: string; model: string; generations: number }[];
  by_virtual_key: { virtual_key_id: string; generations: number }[];
  refusals: {
    limit_source: string;
    reason: string;
    status_code: number;
    requests: number;
  }[];
  timings: {
    measured: number;
    streamed: number;
    /** Streams: to the first chunk; otherwise the whole latency. */
    time_to_first_token_ms: GatewayLatencySummary;
    latency_ms: GatewayLatencySummary;
    generation_time_ms_avg: number;
    throughput_tokens_per_second_avg: number;
  };
}

/** One attempt of a request: the first, or a fallback. No content. */
export interface GatewayAttempt {
  id: string;
  parent_request_id?: string;
  fallback_index: number;
  timestamp: string;
  provider: string;
  model: string;
  /** "success", "error" or "cancelled". */
  status: string;
  /** The HTTP status the attempt ended with: 200 on success. */
  status_code?: number;
  error_type?: string;
  /** Set when the product refused the attempt (the API-08 contract). */
  limit_source?: string;
  reason?: string;
  stream: boolean;
  latency?: number;
  upstream_latency?: number;
  time_to_first_token?: number;
  customer_id?: string;
}

/**
 * One logged request as staff may see it: what happened, never what was said.
 * The gateway's row also carries the prompt and the answer; they are left out
 * where the row is read.
 */
export interface GatewayRequest {
  id: string;
  timestamp: string;
  status: string;
  provider: string;
  model: string;
  customerId: string | null;
  providerKeyName: string | null;
  isStream: boolean;
  latencyMs: number | null;
  promptTokens: number | null;
  completionTokens: number | null;
  /** Provider cost in USD, before our fee. */
  costUsd: number | null;
  stopReason: string | null;
  /** The provider's or the gateway's error message, when the request failed. */
  error: string | null;
}

export interface GatewayRequestSearch {
  customerIds?: string[];
  status?: "success" | "error";
  limit: number;
  offset: number;
}

interface LogRowResponse {
  id?: string;
  timestamp?: string;
  status?: string;
  provider?: string;
  model?: string;
  customer_id?: string | null;
  selected_key_name?: string | null;
  stream?: boolean;
  latency?: number | null;
  token_usage?: {
    prompt_tokens?: number;
    completion_tokens?: number;
  } | null;
  cost?: number | null;
  stop_reason?: string | null;
  error_details?: { error?: { message?: string } } | null;
}

/** What the gateway logged for one customer over a period. Cost is provider cost in USD. */
export interface CustomerCost {
  /** The gateway customer id, or "unassigned" for requests it could not attribute. */
  customerId: string;
  requests: number;
  costUsd: number;
}

interface RankingsResponse {
  rankings?: { id: string; total_requests?: number; total_cost?: number }[];
}

interface ModelDetailsResponse {
  models?: GatewayModel[];
  total?: number;
}

const MODEL_PAGE_SIZE = 500;

/**
 * What a virtual key may reach (docs/platform/adr/0015-data-policy.md):
 * everything, or only the listed provider keys. An empty list reaches
 * nothing: the gateway refuses every request of the key.
 */
export type VirtualKeyAccess =
  | { allowAll: true }
  | { allowAll: false; providers: { provider: string; keyIds: string[] }[] };

const accessBody = (access: VirtualKeyAccess) => ({
  allow_all_providers: access.allowAll,
  // sent without ids: the gateway replaces the key's whole list with these
  provider_configs: access.allowAll
    ? []
    : access.providers.map(({ provider, keyIds }) => ({
        provider,
        allowed_models: ["*"],
        key_ids: keyIds,
      })),
});

/** One provider key in the gateway, without its secret. */
export interface GatewayProviderKey {
  /** The gateway's key_id: what a virtual key's allowlist names. */
  id: string;
  name: string;
  provider: string;
  isEnabled: boolean;
}

interface ProviderKeyResponse {
  key_id?: string;
  name?: string;
  provider?: string;
  enabled?: boolean | null;
}

/** Per-minute ceilings for a customer; null fields are no limit of that kind. */
export interface CustomerRateLimit {
  requestsPerMinute: number | null;
  tokensPerMinute: number | null;
}

interface CustomerResponse {
  customer: { id: string };
}

interface VirtualKeyResponse {
  virtual_key: { id: string; value: string };
}

/** A provider as the gateway holds it, with its keys. No secret is in here. */
export interface GatewayProvider {
  name: string;
  /** Set for a provider of our own that speaks another provider's protocol. */
  baseProvider: string | null;
  baseUrl: string | null;
  status: string;
  keys: GatewayProviderKeyDetails[];
}

export interface GatewayProviderKeyDetails {
  id: string;
  name: string;
  /** The key as the gateway shows it: masked, never the secret itself. */
  maskedValue: string;
  models: string[];
  weight: number;
  isEnabled: boolean;
}

export interface NewGatewayProvider {
  name: string;
  /** With a base provider this is a provider of our own behind `baseUrl`. */
  baseProvider?: string;
  baseUrl?: string;
}

export interface NewGatewayProviderKey {
  name: string;
  value: string;
  models?: string[];
  weight?: number;
}

/** Only the fields named are changed; the gateway keeps the rest, the value included. */
export interface GatewayProviderKeyChange {
  name?: string;
  value?: string;
  models?: string[];
  weight?: number;
  isEnabled?: boolean;
}

interface ProviderResponse {
  name?: string;
  provider_status?: string;
  network_config?: { base_url?: string };
  custom_provider_config?: { base_provider_type?: string } | null;
}

interface ProviderKeyDetailsResponse {
  id?: string;
  name?: string;
  value?: { value?: string };
  models?: string[] | null;
  weight?: number;
  enabled?: boolean;
}

const toProviderKey = (
  key: ProviderKeyDetailsResponse,
): GatewayProviderKeyDetails => ({
  id: key.id ?? "",
  name: key.name ?? "",
  maskedValue: key.value?.value ?? "",
  models: key.models ?? [],
  weight: key.weight ?? 1,
  isEnabled: key.enabled ?? true,
});

/**
 * The scopes the control plane prices at (docs/platform/adr/0018-model-price-table.md):
 * a provider's price, or one provider key's negotiated price.
 */
export type PricingOverrideScope =
  | { kind: "provider"; provider: string }
  | { kind: "provider_key"; providerKeyId: string };

/** A pricing override as the control plane writes it. Rates are USD per unit, by the gateway's field names. */
export interface PricingOverrideInput {
  name: string;
  scope: PricingOverrideScope;
  /** Exact model name; the gateway matches it in any casing. */
  model: string;
  /** Base request types (no stream variants): the gateway applies the patch to these only. */
  requestTypes: readonly string[];
  patch: Record<string, number>;
}

/** A pricing override as the gateway holds it, whoever wrote it. */
export interface GatewayPricingOverride {
  id: string;
  name: string;
  scopeKind: string;
  providerId: string | null;
  providerKeyId: string | null;
  matchType: string;
  pattern: string;
  requestTypes: string[];
  /** The patch as the gateway stored it: only the fields it knows survive. */
  patch: Record<string, number>;
}

interface PricingOverrideResponse {
  id?: string;
  name?: string;
  scope_kind?: string;
  provider_id?: string | null;
  provider_key_id?: string | null;
  match_type?: string;
  pattern?: string;
  request_types?: string[] | null;
  pricing_patch?: string;
}

const pricingOverrideBody = ({
  name,
  scope,
  model,
  requestTypes,
  patch,
}: PricingOverrideInput) => ({
  name,
  scope_kind: scope.kind,
  ...(scope.kind === "provider"
    ? { provider_id: scope.provider, provider_key_id: null }
    : { provider_key_id: scope.providerKeyId, provider_id: null }),
  user_id: null,
  virtual_key_id: null,
  match_type: "exact",
  pattern: model,
  request_types: [...requestTypes],
  patch,
});

const parsePatch = (raw: string | undefined): Record<string, number> => {
  if (!raw) {
    return {};
  }
  const parsed: unknown = JSON.parse(raw);
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return {};
  }
  return Object.fromEntries(
    Object.entries(parsed).filter(
      (entry): entry is [string, number] => typeof entry[1] === "number",
    ),
  );
};

const toPricingOverride = (
  row: PricingOverrideResponse,
): GatewayPricingOverride => ({
  id: row.id ?? "",
  name: row.name ?? "",
  scopeKind: row.scope_kind ?? "",
  providerId: row.provider_id ?? null,
  providerKeyId: row.provider_key_id ?? null,
  matchType: row.match_type ?? "",
  pattern: row.pattern ?? "",
  requestTypes: row.request_types ?? [],
  patch: parsePatch(row.pricing_patch),
});

/**
 * One row of the gateway's datasheet: the upstream price list as synced
 * (`upstream`) and after the gateway's own edits (`effective`, what it
 * charges absent a pricing override). Cost fields are USD per unit, by the
 * datasheet's names.
 */
export interface GatewayDatasheetEntry {
  provider: string;
  model: string;
  mode: string;
  isCustom: boolean;
  upstream: Record<string, unknown> | null;
  effective: Record<string, unknown> | null;
}

interface DatasheetPageResponse {
  entries?: {
    provider?: string;
    model?: string;
    mode?: string;
    is_custom?: boolean;
    upstream?: Record<string, unknown> | null;
    effective?: Record<string, unknown> | null;
  }[];
  total_count?: number;
}

const DATASHEET_PAGE_SIZE = 500;

export const createGatewayClient = ({
  url,
  username,
  password,
  timeoutMs = 10_000,
  fetch: fetchImpl = fetch,
}: GatewayClientOptions) => {
  const base = url.replace(/\/+$/, "");
  const authorization =
    username && password
      ? `Basic ${Buffer.from(`${username}:${password}`).toString("base64")}`
      : undefined;

  const call = async <T>(
    method: string,
    path: string,
    body?: unknown,
    { allowNotFound = false } = {},
  ): Promise<T | undefined> => {
    let response: Response;
    try {
      response = await fetchImpl(`${base}${path}`, {
        method,
        headers: {
          ...(body === undefined ? {} : { "Content-Type": "application/json" }),
          ...(authorization ? { Authorization: authorization } : {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(timeoutMs),
      });
    } catch (error) {
      throw new GatewayError(
        `gateway ${method} ${path} failed: ${error instanceof Error ? error.message : String(error)}`,
      );
    }
    if (allowNotFound && response.status === 404) {
      return undefined;
    }
    if (!response.ok) {
      // the body names the problem; it never carries a key value on an error
      const detail = (await response.text().catch(() => "")).slice(0, 500);
      throw new GatewayError(
        `gateway ${method} ${path} answered ${response.status}: ${detail}`,
        response.status,
      );
    }
    return (await response.json()) as T;
  };

  return {
    /** The gateway generates the id; the caller records which organization it is. */
    createCustomer: async (name: string) => {
      const created = await call<CustomerResponse>(
        "POST",
        "/api/governance/customers",
        { name },
      );
      if (!created?.customer.id) {
        throw new GatewayError("gateway created a customer without an id");
      }
      return { id: created.customer.id };
    },

    /** A customer that is already gone counts as deleted. */
    deleteCustomer: async (id: string) => {
      await call(
        "DELETE",
        `/api/governance/customers/${encodeURIComponent(id)}`,
        undefined,
        { allowNotFound: true },
      );
    },

    /**
     * Every model of what `access` allows (by default every provider the
     * gateway has), and no gateway budget: the prepaid balance and the key's
     * spending limit are both enforced on what the ledger charges
     * (docs/platform/adr/0013-key-limits-in-the-ledger.md).
     */
    createVirtualKey: async ({
      name,
      customerId,
      expiresAt,
      access = { allowAll: true },
    }: CreateVirtualKeyInput): Promise<CreatedVirtualKey> => {
      const created = await call<VirtualKeyResponse>(
        "POST",
        "/api/governance/virtual-keys",
        {
          name,
          customer_id: customerId,
          ...accessBody(access),
          is_active: true,
          ...(expiresAt ? { expires_at: expiresAt.toISOString() } : {}),
        },
      );
      const key = created?.virtual_key;
      if (!key?.id || !key.value) {
        throw new GatewayError(
          "gateway created a virtual key without an id or value",
        );
      }
      return { id: key.id, value: key.value };
    },

    updateVirtualKey: async (id: string, input: UpdateVirtualKeyInput) => {
      await call(
        "PUT",
        `/api/governance/virtual-keys/${encodeURIComponent(id)}`,
        {
          ...(input.name === undefined ? {} : { name: input.name }),
          ...(input.isActive === undefined
            ? {}
            : { is_active: input.isActive }),
          ...(input.expiresAt === undefined
            ? {}
            : { expires_at: input.expiresAt.toISOString() }),
        },
      );
    },

    /** Replaces what the key may reach. A key that is gone is an error. */
    setVirtualKeyAccess: async (id: string, access: VirtualKeyAccess) => {
      await call(
        "PUT",
        `/api/governance/virtual-keys/${encodeURIComponent(id)}`,
        accessBody(access),
      );
    },

    /**
     * Replaces the customer's rate limit, in one-minute windows; null (or both
     * fields null) removes it. The gateway refuses a request over it with 429.
     */
    setCustomerRateLimit: async (
      id: string,
      limit: CustomerRateLimit | null,
    ) => {
      const requests = limit?.requestsPerMinute ?? null;
      const tokens = limit?.tokensPerMinute ?? null;
      await call("PUT", `/api/governance/customers/${encodeURIComponent(id)}`, {
        // both maximums absent is the gateway's "remove the rate limit"
        rate_limit: {
          ...(requests === null
            ? {}
            : { request_max_limit: requests, request_reset_duration: "1m" }),
          ...(tokens === null
            ? {}
            : { token_max_limit: tokens, token_reset_duration: "1m" }),
        },
      });
    },

    /** The gateway's log of one request, or undefined when it has none (or it expired). */
    getLog: async (id: string): Promise<GatewayLog | undefined> =>
      call<GatewayLog>(
        "GET",
        `/api/logs/${encodeURIComponent(id)}`,
        undefined,
        { allowNotFound: true },
      ),

    /** One customer's generations, refusals and timings in [from, to). */
    getGenerationSummary: async (
      customerId: string,
      from: Date,
      to: Date,
    ): Promise<GatewayGenerationSummary> => {
      const query = new URLSearchParams({
        customer_id: customerId,
        start_time: from.toISOString(),
        end_time: to.toISOString(),
      });
      const summary = await call<GatewayGenerationSummary>(
        "GET",
        `/api/logs/generations/summary?${query.toString()}`,
        undefined,
      );
      if (!summary) {
        throw new GatewayError("The gateway returned no summary.", 502);
      }
      return summary;
    },

    /**
     * Every endpoint's attempts in [from, to) by UTC hour. Both must be on the
     * hour, so no hour comes back in part; at most 31 days apart.
     */
    getEndpointStats: async (
      from: Date,
      to: Date,
    ): Promise<GatewayEndpointStats> => {
      const query = new URLSearchParams({
        start_time: from.toISOString(),
        end_time: to.toISOString(),
      });
      const stats = await call<GatewayEndpointStats>(
        "GET",
        `/api/logs/endpoints/hourly?${query.toString()}`,
        undefined,
      );
      if (!stats) {
        throw new GatewayError("The gateway returned no statistics.", 502);
      }
      return { bounds: stats.bounds, hours: stats.hours ?? [] };
    },

    /** A request's attempts, first then fallbacks; empty once its log has expired. */
    getAttempts: async (id: string): Promise<GatewayAttempt[]> => {
      const result = await call<{ attempts?: GatewayAttempt[] | null }>(
        "GET",
        `/api/logs/generations/${encodeURIComponent(id)}/attempts`,
        undefined,
      );
      return result?.attempts ?? [];
    },

    /**
     * Logged requests, newest first. Only the fields named here are passed on:
     * the gateway's rows hold the prompt and the answer, and neither leaves
     * this function.
     */
    searchRequests: async ({
      customerIds,
      status,
      limit,
      offset,
    }: GatewayRequestSearch): Promise<{
      requests: GatewayRequest[];
      total: number;
    }> => {
      const query = new URLSearchParams({
        limit: String(limit),
        offset: String(offset),
        sort_by: "timestamp",
        order: "desc",
      });
      if (customerIds?.length) {
        query.set("customer_ids", customerIds.join(","));
      }
      if (status) {
        query.set("status", status);
      }
      const result = await call<{
        logs?: LogRowResponse[] | null;
        pagination?: { total_count?: number };
      }>("GET", `/api/logs?${query.toString()}`, undefined);
      return {
        requests: (result?.logs ?? []).map((row) => ({
          id: row.id ?? "",
          timestamp: row.timestamp ?? "",
          status: row.status ?? "",
          provider: row.provider ?? "",
          model: row.model ?? "",
          customerId: row.customer_id || null,
          providerKeyName: row.selected_key_name || null,
          isStream: row.stream ?? false,
          latencyMs: row.latency ?? null,
          promptTokens: row.token_usage?.prompt_tokens ?? null,
          completionTokens: row.token_usage?.completion_tokens ?? null,
          costUsd: row.cost ?? null,
          stopReason: row.stop_reason || null,
          error: row.error_details?.error?.message ?? null,
        })),
        total: result?.pagination?.total_count ?? 0,
      };
    },

    /** Logged requests and provider cost per customer, from `from` up to `to`. */
    getCustomerCosts: async (from: Date, to: Date): Promise<CustomerCost[]> => {
      const query = new URLSearchParams({
        dimension: "customer",
        start_time: from.toISOString(),
        end_time: to.toISOString(),
        // the gateway returns 100 rows unless asked for all
        all: "true",
      });
      const result = await call<RankingsResponse>(
        "GET",
        `/api/logs/rankings/by-dimension?${query.toString()}`,
        undefined,
      );
      return (result?.rankings ?? []).map((row) => ({
        customerId: row.id,
        requests: row.total_requests ?? 0,
        costUsd: row.total_cost ?? 0,
      }));
    },

    /**
     * Every model of every configured provider, with its catalog price.
     *
     * With `keyIds`, the gateway narrows each provider's models to those the
     * given provider keys serve and names them in `accessible_by_keys`. A
     * provider that has none of the keys comes back unfiltered, without
     * that field: callers that filter must require it.
     */
    listModels: async (keyIds?: string[]): Promise<GatewayModel[]> => {
      const filter = keyIds?.length
        ? `&keys=${encodeURIComponent(keyIds.join(","))}`
        : "";
      const models: GatewayModel[] = [];
      for (let offset = 0; ; offset += MODEL_PAGE_SIZE) {
        const page = await call<ModelDetailsResponse>(
          "GET",
          `/api/models/details?limit=${MODEL_PAGE_SIZE}&offset=${offset}${filter}`,
          undefined,
        );
        const rows = page?.models ?? [];
        models.push(...rows);
        if (
          rows.length < MODEL_PAGE_SIZE ||
          models.length >= (page?.total ?? 0)
        ) {
          return models;
        }
      }
    },

    /**
     * Every provider key the gateway holds. Only the fields named here are
     * passed on: the gateway redacts the secret, and it never leaves this
     * function either way.
     */
    listProviderKeys: async (): Promise<GatewayProviderKey[]> => {
      const rows = await call<ProviderKeyResponse[]>(
        "GET",
        "/api/keys",
        undefined,
      );
      return (rows ?? []).flatMap((row) =>
        row.key_id && row.provider
          ? [
              {
                id: row.key_id,
                name: row.name ?? "",
                provider: row.provider,
                isEnabled: row.enabled ?? true,
              },
            ]
          : [],
      );
    },

    /** Every provider with its keys. One request per provider for the keys. */
    listProviders: async (): Promise<GatewayProvider[]> => {
      const list = await call<{ providers?: ProviderResponse[] }>(
        "GET",
        "/api/providers",
        undefined,
      );
      return Promise.all(
        (list?.providers ?? []).flatMap((provider) => {
          const name = provider.name;
          if (!name) {
            return [];
          }
          return [
            call<{ keys?: ProviderKeyDetailsResponse[] }>(
              "GET",
              `/api/providers/${encodeURIComponent(name)}/keys`,
              undefined,
            ).then((keys) => ({
              name,
              baseProvider:
                provider.custom_provider_config?.base_provider_type ?? null,
              baseUrl: provider.network_config?.base_url || null,
              status: provider.provider_status ?? "",
              keys: (keys?.keys ?? []).map(toProviderKey),
            })),
          ];
        }),
      );
    },

    /** The gateway checks the name and the address; its refusal comes back as a GatewayError. */
    createProvider: async ({
      name,
      baseProvider,
      baseUrl,
    }: NewGatewayProvider) => {
      await call("POST", "/api/providers", {
        provider: name,
        ...(baseProvider
          ? { custom_provider_config: { base_provider_type: baseProvider } }
          : {}),
        ...(baseUrl ? { network_config: { base_url: baseUrl } } : {}),
      });
    },

    /** Deletes the provider's keys with it. A provider that is already gone counts as deleted. */
    deleteProvider: async (name: string) => {
      await call(
        "DELETE",
        `/api/providers/${encodeURIComponent(name)}`,
        undefined,
        { allowNotFound: true },
      );
    },

    /** The value goes to the gateway and nowhere else; what comes back is masked. */
    createProviderKey: async (
      provider: string,
      { name, value, models, weight }: NewGatewayProviderKey,
    ): Promise<GatewayProviderKeyDetails> => {
      const key = await call<ProviderKeyDetailsResponse>(
        "POST",
        `/api/providers/${encodeURIComponent(provider)}/keys`,
        { name, value, models: models ?? ["*"], weight: weight ?? 1 },
      );
      return toProviderKey(key ?? {});
    },

    updateProviderKey: async (
      provider: string,
      keyId: string,
      { name, value, models, weight, isEnabled }: GatewayProviderKeyChange,
    ): Promise<GatewayProviderKeyDetails> => {
      const key = await call<ProviderKeyDetailsResponse>(
        "PUT",
        `/api/providers/${encodeURIComponent(provider)}/keys/${encodeURIComponent(keyId)}`,
        {
          ...(name === undefined ? {} : { name }),
          ...(value === undefined ? {} : { value }),
          ...(models === undefined ? {} : { models }),
          ...(weight === undefined ? {} : { weight }),
          ...(isEnabled === undefined ? {} : { enabled: isEnabled }),
        },
      );
      return toProviderKey(key ?? {});
    },

    /** Every pricing override the gateway holds, whoever wrote it. */
    listPricingOverrides: async (): Promise<GatewayPricingOverride[]> => {
      const result = await call<{
        pricing_overrides?: PricingOverrideResponse[] | null;
      }>("GET", "/api/governance/pricing-overrides", undefined);
      return (result?.pricing_overrides ?? []).map(toPricingOverride);
    },

    /** The gateway applies it to new requests before it answers. */
    createPricingOverride: async (
      input: PricingOverrideInput,
    ): Promise<GatewayPricingOverride> => {
      const created = await call<{
        pricing_override?: PricingOverrideResponse;
      }>(
        "POST",
        "/api/governance/pricing-overrides",
        pricingOverrideBody(input),
      );
      if (!created?.pricing_override?.id) {
        throw new GatewayError(
          "gateway created a pricing override without an id",
        );
      }
      return toPricingOverride(created.pricing_override);
    },

    /** Replaces the override whole, its patch included. One that is gone is a 404 GatewayError. */
    updatePricingOverride: async (
      id: string,
      input: PricingOverrideInput,
    ): Promise<GatewayPricingOverride> => {
      const updated = await call<{
        pricing_override?: PricingOverrideResponse;
      }>(
        "PUT",
        `/api/governance/pricing-overrides/${encodeURIComponent(id)}`,
        pricingOverrideBody(input),
      );
      return toPricingOverride(updated?.pricing_override ?? { id });
    },

    /** One that is already gone counts as deleted. */
    deletePricingOverride: async (id: string) => {
      await call(
        "DELETE",
        `/api/governance/pricing-overrides/${encodeURIComponent(id)}`,
        undefined,
        { allowNotFound: true },
      );
    },

    /** Every datasheet row of one provider, as the gateway synced it and as it charges it. */
    listDatasheetEntries: async (
      provider: string,
    ): Promise<GatewayDatasheetEntry[]> => {
      const entries: GatewayDatasheetEntry[] = [];
      for (let offset = 0; ; offset += DATASHEET_PAGE_SIZE) {
        const query = new URLSearchParams({
          provider,
          limit: String(DATASHEET_PAGE_SIZE),
          offset: String(offset),
        });
        const page = await call<DatasheetPageResponse>(
          "GET",
          `/api/models/datasheet?${query.toString()}`,
          undefined,
        );
        const rows = page?.entries ?? [];
        entries.push(
          ...rows.map((row) => ({
            provider: row.provider ?? "",
            model: row.model ?? "",
            mode: row.mode ?? "",
            isCustom: row.is_custom ?? false,
            upstream: row.upstream ?? null,
            effective: row.effective ?? null,
          })),
        );
        if (
          rows.length < DATASHEET_PAGE_SIZE ||
          entries.length >= (page?.total_count ?? 0)
        ) {
          return entries;
        }
      }
    },

    /** Reads the price list again now, instead of waiting for the next scheduled sync. */
    syncPrices: async () => {
      await call("POST", "/api/pricing/force-sync", {});
    },

    /** A key that is already gone counts as deleted. */
    deleteProviderKey: async (provider: string, keyId: string) => {
      await call(
        "DELETE",
        `/api/providers/${encodeURIComponent(provider)}/keys/${encodeURIComponent(keyId)}`,
        undefined,
        { allowNotFound: true },
      );
    },

    /** Asks the provider which models it serves now, instead of waiting for the next sync. */
    refreshProviderModels: async (provider: string) => {
      await call(
        "POST",
        `/api/providers/${encodeURIComponent(provider)}/refresh-models`,
        {},
      );
    },

    /** A key that is already gone counts as deleted. */
    deleteVirtualKey: async (id: string) => {
      await call(
        "DELETE",
        `/api/governance/virtual-keys/${encodeURIComponent(id)}`,
        undefined,
        { allowNotFound: true },
      );
    },
  };
};

export type GatewayClient = ReturnType<typeof createGatewayClient>;
