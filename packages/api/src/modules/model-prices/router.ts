import { Hono } from "hono";

import { getGateway } from "@workspace/gateway-client/server";

import { validate } from "../../middleware";
import { withGateway } from "./gateway";
import { importFromGateway } from "./import";
import {
  createPriceVersion,
  getPriceHistory,
  listCurrentPrices,
} from "./prices";
import {
  checkModelPrices,
  getPushStates,
  pushDuePrices,
  pushNow,
  syncModelPrices,
} from "./push";
import {
  approveSuggestionSchema,
  createModelPriceSchema,
  listModelPricesQuerySchema,
  listSuggestionsQuerySchema,
  modelPriceHistoryQuerySchema,
  rejectSuggestionSchema,
  suggestionIdSchema,
} from "./schema";
import {
  approveSuggestion,
  listSuggestions,
  rejectSuggestion,
  syncSuggestions,
} from "./suggestions";

import type { AuditChange } from "../admin/access-middleware";
import type { User } from "@workspace/auth";

/**
 * Pushes the target's price now, after a save. The save stands whether the
 * push works or not: a failure is kept on the target's push state and the
 * scheduled job retries it.
 */
const pushAfterSave = async (target: {
  provider: string;
  model: string;
  providerKeyId: string | null;
}) => {
  try {
    return await pushNow(getGateway(), target);
  } catch (error) {
    return { error: error instanceof Error ? error.message : String(error) };
  }
};

/**
 * Staff's model price table (ADMIN-09), mounted under /api/admin/model-prices
 * behind enforceAdmin; access in admin/access.ts (pricing: read to see,
 * update to change). Each change sets auditChange with before and after.
 */
export const modelPricesAdminRouter = new Hono<{
  Variables: { user: User; auditChange?: AuditChange };
}>()
  .get("/", validate("query", listModelPricesQuerySchema), async (c) => {
    const [list, states] = await Promise.all([
      listCurrentPrices(c.req.valid("query")),
      getPushStates(),
    ]);
    return c.json({ ...list, pushStates: states });
  })
  .get("/history", validate("query", modelPriceHistoryQuerySchema), async (c) =>
    c.json(await getPriceHistory(c.req.valid("query"))),
  )
  .get(
    "/suggestions",
    validate("query", listSuggestionsQuerySchema),
    async (c) => c.json(await listSuggestions(c.req.valid("query"))),
  )
  .get("/check", async (c) =>
    c.json(await withGateway((gateway) => checkModelPrices(gateway))),
  )
  .post("/", validate("json", createModelPriceSchema), async (c) => {
    const { version, before, after } = await createPriceVersion({
      ...c.req.valid("json"),
      staffId: c.var.user.id,
    });
    c.set("auditChange", { before, after });
    const push =
      version.effectiveFrom.getTime() <= Date.now()
        ? await pushAfterSave(version)
        : { scheduled: true };
    return c.json({ version, push });
  })
  .post("/import", async (c) => {
    const result = await withGateway(async (gateway) => {
      const imported = await importFromGateway({
        gateway,
        staffId: c.var.user.id,
      });
      const pushed = await pushDuePrices(gateway);
      return { ...imported, pushed };
    });
    c.set("auditChange", {
      before: null,
      after: { imported: result.imported, existing: result.existing },
    });
    return c.json(result);
  })
  .post("/push", async (c) => {
    const result = await withGateway((gateway) => syncModelPrices(gateway));
    c.set("auditChange", {
      before: null,
      after: { pushed: result.pushed, repaired: result.repaired ?? null },
    });
    return c.json(result);
  })
  .post("/suggestions/sync", async (c) => {
    const result = await withGateway((gateway) => syncSuggestions(gateway));
    c.set("auditChange", { before: null, after: result });
    return c.json(result);
  })
  .post(
    "/suggestions/:id/approve",
    validate("param", suggestionIdSchema),
    validate("json", approveSuggestionSchema),
    async (c) => {
      const { version, before, after } = await approveSuggestion({
        id: c.req.valid("param").id,
        staffId: c.var.user.id,
        ...c.req.valid("json"),
      });
      c.set("auditChange", { before, after });
      const push =
        version.effectiveFrom.getTime() <= Date.now()
          ? await pushAfterSave(version)
          : { scheduled: true };
      return c.json({ version, push });
    },
  )
  .post(
    "/suggestions/:id/reject",
    validate("param", suggestionIdSchema),
    validate("json", rejectSuggestionSchema),
    async (c) => {
      const { before, after } = await rejectSuggestion({
        id: c.req.valid("param").id,
        staffId: c.var.user.id,
        reason: c.req.valid("json").reason,
      });
      c.set("auditChange", { before, after });
      return c.json(after);
    },
  );
