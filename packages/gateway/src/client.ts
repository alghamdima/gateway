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
  /** Milliseconds. */
  latency?: number;
  timestamp?: string;
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

    /** Every model of every configured provider, with its catalog price. */
    listModels: async (): Promise<GatewayModel[]> => {
      const models: GatewayModel[] = [];
      for (let offset = 0; ; offset += MODEL_PAGE_SIZE) {
        const page = await call<ModelDetailsResponse>(
          "GET",
          `/api/models/details?limit=${MODEL_PAGE_SIZE}&offset=${offset}`,
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
