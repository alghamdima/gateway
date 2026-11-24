import * as z from "zod";

import { handle } from "@workspace/api/utils";

import { api } from "~/lib/api/server";

/** What GET /api/v1/models returns, as far as these pages read it. */
const catalogSchema = z.object({
  data: z.array(
    z.object({
      id: z.string(),
      name: z.string(),
      context_length: z.number().nullable(),
      architecture: z.object({
        input_modalities: z.array(z.string()),
        output_modalities: z.array(z.string()),
      }),
      pricing: z.object({
        prompt: z.string(),
        completion: z.string(),
        input_cache_read: z.string().optional(),
        input_cache_write: z.string().optional(),
      }),
      top_provider: z.object({
        max_completion_tokens: z.number().nullable(),
      }),
    }),
  ),
});

/**
 * The public model catalog (step 8.3), read from GET /api/v1/models: the
 * same list and the same prices an API client gets. Null when the catalog
 * cannot be read, so a page can say so instead of failing.
 */
export const getCatalog = async () => {
  try {
    const { data } = await handle(api.v1.models.$get, {
      schema: catalogSchema,
    })();
    return data;
  } catch {
    return null;
  }
};

export type CatalogModel = z.infer<typeof catalogSchema>["data"][number];

export const providerOf = (model: CatalogModel) =>
  model.id.slice(0, model.id.indexOf("/"));

const TOKENS_PER_MILLION = 1_000_000;

/** A price per token as USD per million tokens, the unit customers compare. */
export const perMillion = (pricePerToken: string, locale: string) =>
  new Intl.NumberFormat(locale, {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 4,
  }).format(Number(pricePerToken) * TOKENS_PER_MILLION);

export const formatCount = (value: number | null, locale: string) =>
  value === null ? null : new Intl.NumberFormat(locale).format(value);
