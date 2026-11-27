import type {
  CreateVirtualKeyInput,
  CustomerRateLimit,
  GatewayClient,
  UpdateVirtualKeyInput,
} from "@workspace/gateway-client/server";

// ids stay unique across tests, like a real gateway's
let next = 0;

/**
 * An in-memory gateway that records every call and can be told to fail, for
 * the keys tests that run against real Postgres.
 */
export const fakeGateway = () => {
  const state = {
    customers: new Set<string>(),
    keys: new Map<string, CreateVirtualKeyInput & { isActive: boolean }>(),
    updates: [] as [string, UpdateVirtualKeyInput][],
    deletedKeys: [] as string[],
    deletedCustomers: [] as string[],
    rateLimits: [] as [string, CustomerRateLimit | null][],
    failRateLimit: false,
    failCreateKey: false,
    /** When set, updating a key fails, as a gateway that is down. */
    failUpdateKey: false,
    /** When set, every new key gets this value (to force a duplicate). */
    fixedValue: undefined as string | undefined,
    /** When set, createCustomer waits until this many calls are waiting. */
    customerBarrier: 0,
  };
  let waiting: (() => void)[] = [];
  const client: GatewayClient = {
    createCustomer: async () => {
      if (state.customerBarrier > 0) {
        await new Promise<void>((resolve) => {
          waiting.push(resolve);
          if (waiting.length >= state.customerBarrier) {
            waiting.forEach((release) => release());
            waiting = [];
          }
        });
      }
      const id = `cust_${++next}`;
      state.customers.add(id);
      return { id };
    },
    deleteCustomer: async (id) => {
      state.customers.delete(id);
      state.deletedCustomers.push(id);
    },
    createVirtualKey: async (input) => {
      if (state.failCreateKey) {
        throw new Error("gateway down");
      }
      // like the real gateway: virtual key names are unique across all customers
      if ([...state.keys.values()].some((key) => key.name === input.name)) {
        const { GatewayError } =
          await import("@workspace/gateway-client/server");
        throw new GatewayError(
          "A virtual key with this name already exists",
          409,
        );
      }
      const id = `vk_${++next}`;
      state.keys.set(id, { ...input, isActive: true });
      return { id, value: state.fixedValue ?? `sk-bf-${id}-${"x".repeat(30)}` };
    },
    updateVirtualKey: async (id, input) => {
      if (state.failUpdateKey) {
        const { GatewayError } =
          await import("@workspace/gateway-client/server");
        throw new GatewayError("gateway down");
      }
      state.updates.push([id, input]);
      const key = state.keys.get(id);
      if (key && input.isActive !== undefined) {
        key.isActive = input.isActive;
      }
      // like the real gateway: an expiry must be in the future
      if (key && input.expiresAt !== undefined) {
        if (input.expiresAt.getTime() <= Date.now()) {
          const { GatewayError } =
            await import("@workspace/gateway-client/server");
          throw new GatewayError("expires_at must be a future timestamp", 400);
        }
        key.expiresAt = input.expiresAt;
      }
    },
    listModels: async () => [],
    searchRequests: async () => ({ requests: [], total: 0 }),
    syncPrices: async () => {},
    listPricingOverrides: async () => [],
    createPricingOverride: async () => {
      throw new Error("not used");
    },
    updatePricingOverride: async () => {
      throw new Error("not used");
    },
    deletePricingOverride: async () => {},
    listDatasheetEntries: async () => [],
    listProviders: async () => [],
    createProvider: async () => {},
    deleteProvider: async () => {},
    createProviderKey: async () => ({
      id: "pk",
      name: "",
      maskedValue: "",
      models: [],
      weight: 1,
      isEnabled: true,
    }),
    updateProviderKey: async () => ({
      id: "pk",
      name: "",
      maskedValue: "",
      models: [],
      weight: 1,
      isEnabled: true,
    }),
    deleteProviderKey: async () => {},
    refreshProviderModels: async () => {},
    listProviderKeys: async () => [],
    setVirtualKeyAccess: async () => {},
    getCustomerCosts: async () => [],
    getGenerationSummary: async () => {
      throw new Error("not used");
    },
    getAttempts: async () => [],
    getLog: async () => undefined,
    setCustomerRateLimit: async (id, limit) => {
      if (state.failRateLimit) {
        // loaded here: a static import would open the database before the test points it at its own
        const { GatewayError } =
          await import("@workspace/gateway-client/server");
        throw new GatewayError("gateway down");
      }
      state.rateLimits.push([id, limit]);
    },
    deleteVirtualKey: async (id) => {
      state.keys.delete(id);
      state.deletedKeys.push(id);
    },
  };
  return { state, client };
};
