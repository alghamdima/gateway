import { Hono } from "hono";
import { createMiddleware } from "hono/factory";

import {
  GatewayError,
  GatewayNotConfiguredError,
} from "@workspace/gateway/server";
import { logger } from "@workspace/shared/logger";

import { authenticateKey, readKey } from "./auth";
import { listPublicModels } from "./models";
import { describeCredits, describeKey } from "./queries";

import type { CallingKey } from "./auth";
import type { ContentfulStatusCode } from "hono/utils/http-status";

/**
 * The OpenRouter-compatible endpoints the control plane answers
 * (docs/platform/PLAN.md, step 8.2). The model endpoints themselves are the
 * gateway's. Errors use OpenRouter's shape: { error: { code, message } }.
 */

const apiError = (status: ContentfulStatusCode, message: string) =>
  Response.json({ error: { code: status, message } }, { status });

const enforceKey = createMiddleware<{ Variables: { key: CallingKey } }>(
  async (c, next) => {
    const sent = readKey({
      authorization: c.req.header("authorization"),
      apiKey: c.req.header("x-api-key"),
    });
    if ("message" in sent) {
      return apiError(401, sent.message);
    }
    const result = await authenticateKey(sent.key);
    if (!result.isAuthenticated) {
      return apiError(401, result.message);
    }
    c.set("key", result.key);
    return next();
  },
);

export const v1Router = new Hono<{ Variables: { key: CallingKey } }>()
  // public, as OpenRouter's: no key needed to see models and prices
  .get("/models", async (c) => {
    try {
      return c.json(await listPublicModels());
    } catch (error) {
      if (
        error instanceof GatewayError ||
        error instanceof GatewayNotConfiguredError
      ) {
        logger.error(error);
        return apiError(
          503,
          "The model catalog is unavailable. Try again shortly.",
        );
      }
      throw error;
    }
  })
  .use("/key", enforceKey)
  .use("/credits", enforceKey)
  .get("/key", async (c) => c.json(await describeKey(c.var.key)))
  .get("/credits", async (c) =>
    c.json(await describeCredits(c.var.key.organizationId)),
  );
