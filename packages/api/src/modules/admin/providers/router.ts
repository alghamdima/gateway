import { Hono } from "hono";
import * as z from "zod";

import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { validate } from "../../../middleware";
import { providerKeyIdSchema, setProviderTermsSchema } from "../../../schema";
import { applyPendingPolicies } from "../../data-policy/policy";
import {
  getProviderCatalog,
  removeProviderTerms,
  setProviderTerms,
} from "./providers";

import type { User } from "@workspace/auth";

const keyParamSchema = z.object({ keyId: providerKeyIdSchema });

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
    const isRemoved = await removeProviderTerms(c.req.valid("param").keyId);
    if (!isRemoved) {
      throw new HttpException(HttpStatusCode.NOT_FOUND, {
        code: "error.notFound",
      });
    }
    return c.json({ isRemoved, ...(await pushPolicies()) });
  });
