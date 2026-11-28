import { getVersionsInForce } from "./prices";

import type { ModelPriceVersion } from "./prices";

/**
 * What customers see (docs/platform/adr/0018-model-price-table.md): our
 * provider-wide price in force for each model, the price the gateway charges
 * through its pricing overrides. The public catalog reads this, never the
 * datasheet. A key's own price is not shown: it is what that key costs us.
 */
export const currentProviderPrices = async (now = new Date()) => {
  const versions = await getVersionsInForce(now, { scope: "provider" });
  const byModel = new Map(
    versions.map((version) => [
      `${version.provider}/${version.model.toLowerCase()}`,
      version,
    ]),
  );
  return {
    get: (provider: string, model: string): ModelPriceVersion | undefined =>
      byModel.get(`${provider}/${model.toLowerCase()}`),
  };
};
