import { getGateway } from "@workspace/gateway/server";

import { marketPrice } from "../pricing/price";
import { getPricing } from "../pricing/pricing";

import type { GatewayClient, GatewayModel } from "@workspace/gateway/server";

/**
 * GET /api/v1/models: the models the gateway can serve, in OpenRouter's
 * shape, with prices that include our markup for the model's provider
 * (ADR 0005, 0010), so the price shown is the price charged.
 */

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
  pricing: {
    prompt: string;
    completion: string;
    input_cache_read?: string;
    input_cache_write?: string;
  };
  top_provider: {
    context_length: number | null;
    max_completion_tokens: number | null;
  };
}

const CACHE_MS = 60_000;
let cached: { at: number; models: PublicModel[] } | undefined;

export const listPublicModels = async (
  gateway: GatewayClient = getGateway(),
  now = Date.now(),
): Promise<{ data: PublicModel[] }> => {
  if (cached && now - cached.at < CACHE_MS) {
    return { data: cached.models };
  }
  const [models, { settings, providers }] = await Promise.all([
    gateway.listModels(),
    getPricing(),
  ]);
  const markupFor = new Map(
    providers.map((row) => [row.provider, row.markupBps]),
  );
  const publicModels = models
    .filter((model) => !model.is_deprecated && hasPrice(model))
    .map((model) =>
      toPublicModel(
        model,
        markupFor.get(model.provider) ?? settings.defaultMarkupBps,
      ),
    )
    .sort((a, b) => a.id.localeCompare(b.id));

  cached = { at: now, models: publicModels };
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

const toPublicModel = (model: GatewayModel, markupBps: number): PublicModel => {
  const price = (field: CostField) => {
    const value = cost(model, field);
    return value === undefined ? undefined : marketPrice(value, markupBps);
  };
  const cacheRead = price("cache_read_input_token_cost");
  const cacheWrite = price("cache_creation_input_token_cost");
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
    pricing: {
      prompt: price("input_cost_per_token") ?? "0",
      completion: price("output_cost_per_token") ?? "0",
      ...(cacheRead === undefined ? {} : { input_cache_read: cacheRead }),
      ...(cacheWrite === undefined ? {} : { input_cache_write: cacheWrite }),
    },
    top_provider: {
      context_length: contextLength,
      max_completion_tokens: model.max_output_tokens ?? null,
    },
  };
};
