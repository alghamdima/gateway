import { Hono } from "hono";
import * as z from "zod";

import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { validate } from "../../../middleware";
import { providerKeyIdSchema, setProviderTermsSchema } from "../../../schema";
import {
  getProviderCatalog,
  removeProviderTerms,
  setProviderTerms,
} from "./providers";

import type { User } from "@workspace/auth";

const keyParamSchema = z.object({ keyId: providerKeyIdSchema });

/** Mounted under /api/admin, behind enforceAdmin. */
export const providersRouter = new Hono<{ Variables: { user: User } }>()
  .get("/", async (c) => c.json(await getProviderCatalog()))
  .put(
    "/:keyId/terms",
    validate("param", keyParamSchema),
    validate("json", setProviderTermsSchema),
    async (c) =>
      c.json(
        await setProviderTerms({
          ...c.req.valid("param"),
          ...c.req.valid("json"),
          staffId: c.var.user.id,
        }),
      ),
  )
  .delete("/:keyId/terms", validate("param", keyParamSchema), async (c) => {
    const isRemoved = await removeProviderTerms(c.req.valid("param").keyId);
    if (!isRemoved) {
      throw new HttpException(HttpStatusCode.NOT_FOUND, {
        code: "error.notFound",
      });
    }
    return c.json({ isRemoved });
  });
