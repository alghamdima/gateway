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

interface ModelDetailsResponse {
  models?: GatewayModel[];
  total?: number;
}

const MODEL_PAGE_SIZE = 500;

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
     * Every provider the gateway has, every model, and no gateway budget: the
     * prepaid balance and the key's spending limit are both enforced on what
     * the ledger charges (docs/platform/adr/0013-key-limits-in-the-ledger.md).
     */
    createVirtualKey: async ({
      name,
      customerId,
      expiresAt,
    }: CreateVirtualKeyInput): Promise<CreatedVirtualKey> => {
      const created = await call<VirtualKeyResponse>(
        "POST",
        "/api/governance/virtual-keys",
        {
          name,
          customer_id: customerId,
          allow_all_providers: true,
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
