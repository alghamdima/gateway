import { getGateway } from "@workspace/gateway-client/server";
import { formatRateE6 } from "@workspace/shared/utils";

import { marketPrice, marketPriceSar } from "../pricing/price";
import { getPricingSettings } from "../pricing/pricing";

import type {
  GatewayClient,
  GatewayModel,
  VirtualKeyAccess,
} from "@workspace/gateway-client/server";

/**
 * GET /api/v1/models: the models the gateway can serve, in OpenRouter's
 * shape, at the serving provider's price with no markup (ADR 0010: the
 * platform's revenue is the top-up fee), so the price shown is the price
 * charged. `pricing` is USD per token, as OpenRouter's; `pricing_sar` is the
 * same in SAR at the current effective USD to SAR rate (ADR 0017), which is
 * what new requests convert at.
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
  const [listed, settings] = await Promise.all([
    gateway.listModels(allowedKeys && [...allowedKeys]),
    getPricingSettings(),
  ]);
  const models = allowedKeys
    ? listed.filter((model) =>
        model.accessible_by_keys?.some((keyId) => allowedKeys.has(keyId)),
      )
    : listed;
  const publicModels = models
    .filter((model) => !model.is_deprecated && hasPrice(model))
    .map((model) => toPublicModel(model, settings.effectiveUsdToSarRateE6))
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

// a model without prompt and completion prices cannot be charged, so the
// credits plugin refuses it (credits_unpriced_model): it is not offered
const hasPrice = (model: GatewayModel) =>
  cost(model, "input_cost_per_token") !== undefined &&
  cost(model, "output_cost_per_token") !== undefined;

type CostField =
  | "input_cost_per_token"
  | "output_cost_per_token"
  | "cache_read_input_token_cost"
  | "cache_creation_input_token_cost";

// an override in the gateway is what CalculateCost charges, so it wins
const cost = (model: GatewayModel, field: CostField) =>
  model.overridden_pricing?.[field] ?? model[field];

const toPublicModel = (
  model: GatewayModel,
  usdToSarRateE6: number,
): PublicModel => {
  const pricing = (toPrice: (value: number) => string): PublicPricing => {
    const price = (field: CostField) => {
      const value = cost(model, field);
      return value === undefined ? undefined : toPrice(value);
    };
    const cacheRead = price("cache_read_input_token_cost");
    const cacheWrite = price("cache_creation_input_token_cost");
    return {
      prompt: price("input_cost_per_token") ?? "0",
      completion: price("output_cost_per_token") ?? "0",
      ...(cacheRead === undefined ? {} : { input_cache_read: cacheRead }),
      ...(cacheWrite === undefined ? {} : { input_cache_write: cacheWrite }),
    };
  };
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
    pricing: pricing(marketPrice),
    pricing_sar: pricing((value) => marketPriceSar(value, usdToSarRateE6)),
    usd_to_sar_rate: formatRateE6(usdToSarRateE6),
    top_provider: {
      context_length: contextLength,
      max_completion_tokens: model.max_output_tokens ?? null,
    },
  };
};
