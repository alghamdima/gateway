import { Hono } from "hono";
import * as z from "zod";

import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { validate } from "../../../middleware";
import {
  createProviderAccountSchema,
  createProviderKeySchema,
  providerKeyIdSchema,
  providerNameSchema,
  setProviderTermsSchema,
  updateProviderKeySchema,
} from "../../../schema";
import { applyPendingPolicies } from "../../data-policy/policy";
import {
  createProviderAccount,
  createProviderKey,
  deleteProviderAccount,
  deleteProviderKey,
  listProviderAccounts,
  refreshProviderModels,
  updateProviderKey,
} from "./accounts";
import {
  getProviderCatalog,
  removeProviderTerms,
  setProviderTerms,
} from "./providers";

import type { User } from "@workspace/auth";

const keyParamSchema = z.object({ keyId: providerKeyIdSchema });
const accountParamSchema = z.object({ provider: providerNameSchema });
const accountKeyParamSchema = accountParamSchema.extend({
  keyId: providerKeyIdSchema,
});

/**
 * A change of terms changes what data policies allow: push them now. The
 * count is the organizations whose keys could not be updated; they stay
 * marked as not applied and the scheduled job retries them.
 */
const pushPolicies = async () => ({
  unappliedPolicies: (await applyPendingPolicies()).failed.length,
});

/** Mounted under /api/admin, behind enforceAdmin. */
export const providersRouter = new Hono<{ Variables: { user: User } }>()
  .get("/", async (c) => c.json(await getProviderCatalog()))
  // the providers themselves and their keys, which live in the gateway
  .get("/accounts", async (c) => c.json(await listProviderAccounts()))
  .post(
    "/accounts",
    validate("json", createProviderAccountSchema),
    async (c) => {
      await createProviderAccount(c.req.valid("json"));
      return c.json({ isCreated: true });
    },
  )
  .delete(
    "/accounts/:provider",
    validate("param", accountParamSchema),
    async (c) => {
      await deleteProviderAccount(c.req.valid("param").provider, c.var.user.id);
      return c.json({ isDeleted: true, ...(await pushPolicies()) });
    },
  )
  .post(
    "/accounts/:provider/refresh-models",
    validate("param", accountParamSchema),
    async (c) => {
      await refreshProviderModels(c.req.valid("param").provider);
      return c.json({ isRefreshed: true });
    },
  )
  .post(
    "/accounts/:provider/keys",
    validate("param", accountParamSchema),
    validate("json", createProviderKeySchema),
    async (c) =>
      c.json(
        await createProviderKey(
          c.req.valid("param").provider,
          c.req.valid("json"),
        ),
      ),
  )
  .put(
    "/accounts/:provider/keys/:keyId",
    validate("param", accountKeyParamSchema),
    validate("json", updateProviderKeySchema),
    async (c) => {
      const { provider, keyId } = c.req.valid("param");
      return c.json(
        await updateProviderKey(provider, keyId, c.req.valid("json")),
      );
    },
  )
  .delete(
    "/accounts/:provider/keys/:keyId",
    validate("param", accountKeyParamSchema),
    async (c) => {
      const { provider, keyId } = c.req.valid("param");
      await deleteProviderKey(provider, keyId, c.var.user.id);
      return c.json({ isDeleted: true, ...(await pushPolicies()) });
    },
  )
  .put(
    "/:keyId/terms",
    validate("param", keyParamSchema),
    validate("json", setProviderTermsSchema),
    async (c) => {
      const terms = await setProviderTerms({
        ...c.req.valid("param"),
        ...c.req.valid("json"),
        staffId: c.var.user.id,
      });
      return c.json({ ...terms, ...(await pushPolicies()) });
    },
  )
  .delete("/:keyId/terms", validate("param", keyParamSchema), async (c) => {
    const isRemoved = await removeProviderTerms(
      c.req.valid("param").keyId,
      c.var.user.id,
    );
    if (!isRemoved) {
      throw new HttpException(HttpStatusCode.NOT_FOUND, {
        code: "error.notFound",
      });
    }
    return c.json({ isRemoved, ...(await pushPolicies()) });
  });
