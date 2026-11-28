import { withGateway } from "./gateway";
import { syncModelPrices } from "./push";
import { syncSuggestions } from "./suggestions";

/**
 * The scheduled jobs of the model price table (ADR 0018), called from
 * /api/internal with the cron secret.
 */

/** /api/internal/model-prices: push what is due, check the gateway, mend drift. Every minute. */
export const runModelPricesJob = () =>
  withGateway((gateway) => syncModelPrices(gateway));

/** /api/internal/model-price-suggestions: compare the datasheet with our prices. Daily. */
export const runModelPriceSuggestionsJob = () =>
  withGateway((gateway) => syncSuggestions(gateway));
