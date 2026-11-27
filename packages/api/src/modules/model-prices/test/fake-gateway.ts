import { GatewayError } from "@workspace/gateway-client/server";

import type {
  GatewayClient,
  GatewayDatasheetEntry,
  GatewayModel,
  GatewayPricingOverride,
  PricingOverrideInput,
} from "@workspace/gateway-client/server";

/**
 * A gateway that holds pricing overrides in memory the way the real one
 * stores them (only numeric patch fields survive), with a model catalog and
 * datasheet rows the test sets. Every write is recorded.
 */
export interface FakeGateway {
  client: GatewayClient;
  overrides: Map<string, GatewayPricingOverride>;
  calls: string[];
  models: GatewayModel[];
  datasheet: GatewayDatasheetEntry[];
  /** Makes the next pricing-override write fail with this status. */
  failNext: (status: number) => void;
}

const notUsed = async (): Promise<never> => {
  throw new Error("not used by the model price tests");
};

export const fakeGateway = (): FakeGateway => {
  const overrides = new Map<string, GatewayPricingOverride>();
  const calls: string[] = [];
  let next = 0;
  let failure: number | undefined;
  const state: FakeGateway = {
    overrides,
    calls,
    models: [],
    datasheet: [],
    failNext: (status) => {
      failure = status;
    },
    client: {
      createCustomer: notUsed,
      deleteCustomer: notUsed,
      createVirtualKey: notUsed,
      updateVirtualKey: notUsed,
      setVirtualKeyAccess: notUsed,
      setCustomerRateLimit: notUsed,
      getLog: notUsed,
      searchRequests: notUsed,
      getCustomerCosts: notUsed,
      getGenerationSummary: notUsed,
      getAttempts: notUsed,
      listModels: async () => state.models,
      listProviderKeys: notUsed,
      listProviders: notUsed,
      createProvider: notUsed,
      deleteProvider: notUsed,
      createProviderKey: notUsed,
      updateProviderKey: notUsed,
      syncPrices: notUsed,
      deleteProviderKey: notUsed,
      refreshProviderModels: notUsed,
      deleteVirtualKey: notUsed,
      listPricingOverrides: async () => [...overrides.values()],
      createPricingOverride: async (input) => {
        calls.push(`create ${input.name}`);
        throwIfFailing();
        const override = stored(`po_${++next}`, input);
        overrides.set(override.id, override);
        return override;
      },
      updatePricingOverride: async (id, input) => {
        calls.push(`update ${id} ${input.name}`);
        throwIfFailing();
        if (!overrides.has(id)) {
          throw new GatewayError(
            `gateway PUT override ${id} answered 404`,
            404,
          );
        }
        const override = stored(id, input);
        overrides.set(id, override);
        return override;
      },
      deletePricingOverride: async (id) => {
        calls.push(`delete ${id}`);
        overrides.delete(id);
      },
      listDatasheetEntries: async (provider) =>
        state.datasheet.filter((entry) => entry.provider === provider),
    },
  };

  const throwIfFailing = () => {
    if (failure !== undefined) {
      const status = failure;
      failure = undefined;
      throw new GatewayError(`gateway answered ${status}`, status);
    }
  };

  return state;
};

/** What the gateway keeps of a write: its own JSON round trip of the patch. */
const stored = (
  id: string,
  input: PricingOverrideInput,
): GatewayPricingOverride => ({
  id,
  name: input.name,
  scopeKind: input.scope.kind,
  providerId: input.scope.kind === "provider" ? input.scope.provider : null,
  providerKeyId:
    input.scope.kind === "provider_key" ? input.scope.providerKeyId : null,
  matchType: "exact",
  pattern: input.model,
  requestTypes: [...input.requestTypes],
  patch: { ...input.patch },
});
