import type {
  GatewayClient,
  GatewayDatasheetEntry,
} from "@workspace/gateway-client/server";

/**
 * The gateway's datasheet rows for the models it offers
 * (docs/platform/adr/0018-model-price-table.md): the source of the import
 * (`effective`, what it charges today) and of suggestions (`upstream`, the
 * provider's list as synced).
 */

/** When a model has rows in several modes, the text ones come first: those are charged today. */
const MODE_ORDER = ["chat", "responses", "completion", "embedding"];
const modeRank = (mode: string) => {
  const rank = MODE_ORDER.indexOf(mode);
  return rank === -1 ? MODE_ORDER.length : rank;
};

export interface OfferedModel {
  /** The provider as the gateway is configured with it (the push's scope). */
  provider: string;
  /** The model as the gateway's catalog names it. */
  model: string;
  entry: GatewayDatasheetEntry | undefined;
}

/**
 * Every model the gateway lists, with its datasheet row. A provider's rows
 * are read once; a model is matched in any casing, as the gateway matches.
 */
export const offeredModels = async (
  gateway: GatewayClient,
): Promise<OfferedModel[]> => {
  const models = await gateway.listModels();
  const providers = [...new Set(models.map((model) => model.provider))].sort();
  const rowsByProvider = new Map<string, Map<string, GatewayDatasheetEntry>>();
  for (const provider of providers) {
    const rows = await gateway.listDatasheetEntries(provider);
    const byModel = new Map<string, GatewayDatasheetEntry>();
    for (const row of [...rows].sort(
      (a, b) => modeRank(a.mode) - modeRank(b.mode),
    )) {
      const key = row.model.toLowerCase();
      if (!byModel.has(key)) {
        byModel.set(key, row);
      }
    }
    rowsByProvider.set(provider, byModel);
  }
  const seen = new Set<string>();
  const offered: OfferedModel[] = [];
  for (const model of models) {
    const key = `${model.provider}/${model.name.toLowerCase()}`;
    if (seen.has(key)) {
      continue;
    }
    seen.add(key);
    offered.push({
      provider: model.provider,
      model: model.name,
      entry: rowsByProvider.get(model.provider)?.get(model.name.toLowerCase()),
    });
  }
  return offered.sort((a, b) =>
    a.provider === b.provider
      ? a.model.localeCompare(b.model)
      : a.provider.localeCompare(b.provider),
  );
};
