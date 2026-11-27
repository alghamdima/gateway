import { getGateway } from "@workspace/gateway-client/server";
import { formatRateE6 } from "@workspace/shared/utils";

import { currentProviderPrices } from "../model-prices/catalog";
import { usdToSar } from "../model-prices/rates";
import { getPricingSettings } from "../pricing/pricing";

import type { ModelPriceVersion } from "../model-prices/prices";
import type {
  GatewayClient,
  GatewayModel,
  VirtualKeyAccess,
} from "@workspace/gateway-client/server";

/**
 * GET /api/v1/models: the models the gateway can serve, in OpenRouter's
 * shape, at our provider-wide price in force (ADMIN-09, ADR 0018), which the
 * gateway charges through its pricing overrides, with no markup (ADR 0010:
 * the platform's revenue is the top-up fee), so the price shown is the price
 * charged. `pricing` is USD per token, as OpenRouter's; `pricing_sar` is the
 * same in SAR at the current effective USD to SAR rate (ADR 0017), which is
 * what new requests convert at. A model the gateway lists without a price of
 * ours is not offered.
 */

export interface PublicPricing {
  prompt: string;
  completion: string;
  input_cache_read?: string;
  input_cache_write?: string;
}

export interface PublicModel {
  id: string;
  name: string;
  context_length: number | null;
  architecture: {
    modality: string | null;
    input_modalities: string[];
    output_modalities: string[];
    tokenizer: string | null;
  };
  pricing: PublicPricing;
  /** The same prices in SAR per token, at `usd_to_sar_rate`. */
  pricing_sar: PublicPricing;
  /** The effective USD to SAR rate the SAR prices use, as a decimal string. */
  usd_to_sar_rate: string;
  top_provider: {
    context_length: number | null;
    max_completion_tokens: number | null;
  };
}

const CACHE_MS = 60_000;
let cached: { at: number; models: PublicModel[] } | undefined;

/**
 * The models on offer. With an `access` that names provider keys (an
 * organization with a data policy, ADR 0015), only the models those keys
 * serve: a model is listed when the gateway says one of them serves it, so a
 * policy nothing satisfies lists nothing. Only the full catalog is cached.
 */
export const listPublicModels = async (
  gateway: GatewayClient = getGateway(),
  now = Date.now(),
  access: VirtualKeyAccess = { allowAll: true },
): Promise<{ data: PublicModel[] }> => {
  if (access.allowAll && cached && now - cached.at < CACHE_MS) {
    return { data: cached.models };
  }
  const allowedKeys = access.allowAll
    ? undefined
    : new Set(access.providers.flatMap((provider) => provider.keyIds));
  if (allowedKeys?.size === 0) {
    return { data: [] };
  }
  const [listed, settings, prices] = await Promise.all([
    gateway.listModels(allowedKeys && [...allowedKeys]),
    getPricingSettings(),
    currentProviderPrices(),
  ]);
  const models = allowedKeys
    ? listed.filter((model) =>
        model.accessible_by_keys?.some((keyId) => allowedKeys.has(keyId)),
      )
    : listed;
  const publicModels = models
    .flatMap((model) => {
      const price = prices.get(model.provider, model.name);
      return !model.is_deprecated && price && hasPrice(price)
        ? [toPublicModel(model, price, settings.effectiveUsdToSarRateE6)]
        : [];
    })
    .sort((a, b) => a.id.localeCompare(b.id));

  if (access.allowAll) {
    cached = { at: now, models: publicModels };
  }
  return { data: publicModels };
};

/** For tests: forget the cached catalog. */
export const clearModelCache = () => {
  cached = undefined;
};

// a model without prompt and completion prices is not offered: the catalog
// shows text models, and the credits plugin refuses what it cannot price
const hasPrice = (price: ModelPriceVersion) =>
  price.inputUsd !== null && price.outputUsd !== null;

const toPublicModel = (
  model: GatewayModel,
  price: ModelPriceVersion,
  usdToSarRateE6: number,
): PublicModel => {
  const pricing = (toPrice: (usd: string) => string): PublicPricing => ({
    prompt: price.inputUsd === null ? "0" : toPrice(price.inputUsd),
    completion: price.outputUsd === null ? "0" : toPrice(price.outputUsd),
    ...(price.cacheReadUsd === null
      ? {}
      : { input_cache_read: toPrice(price.cacheReadUsd) }),
    ...(price.cacheWriteUsd === null
      ? {}
      : { input_cache_write: toPrice(price.cacheWriteUsd) }),
  });
  const contextLength = model.context_length ?? model.max_input_tokens ?? null;

  return {
    id: `${model.provider}/${model.name}`,
    name: model.name,
    context_length: contextLength,
    architecture: {
      modality: model.architecture?.modality ?? null,
      input_modalities: model.architecture?.input_modalities ?? [],
      output_modalities: model.architecture?.output_modalities ?? [],
      tokenizer: model.architecture?.tokenizer ?? null,
    },
    pricing: pricing((usd) => usd),
    pricing_sar: pricing((usd) => usdToSar(usd, usdToSarRateE6)),
    usd_to_sar_rate: formatRateE6(usdToSarRateE6),
    top_provider: {
      context_length: contextLength,
      max_completion_tokens: model.max_output_tokens ?? null,
    },
  };
};
