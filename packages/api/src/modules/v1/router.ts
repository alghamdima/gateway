import { Hono } from "hono";
import { createMiddleware } from "hono/factory";

import {
  GatewayError,
  GatewayNotConfiguredError,
} from "@workspace/gateway/server";
import { logger } from "@workspace/shared/logger";

import { getGeneration } from "../activity/generations";
import { accessForOrganization } from "../data-policy/policy";
import { AUTH_MESSAGES, authenticateKey, readKey } from "./auth";
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
  // public, as OpenRouter's: no key needed to see models and prices. With a
  // key, the list is what that organization's data policy can reach (ADR 0015);
  // a key that is not valid is refused rather than shown the whole catalog.
  .get("/models", async (c) => {
    const sent = readKey({
      authorization: c.req.header("authorization"),
      apiKey: c.req.header("x-api-key"),
    });
    // no credentials at all is the public catalog; anything else must be a valid key
    if ("message" in sent && sent.message !== AUTH_MESSAGES.missing) {
      return apiError(401, sent.message);
    }
    const caller = "key" in sent ? await authenticateKey(sent.key) : undefined;
    if (caller && !caller.isAuthenticated) {
      return apiError(401, caller.message);
    }
    try {
      return c.json(
        await listPublicModels(
          undefined,
          undefined,
          caller
            ? await accessForOrganization(caller.key.organizationId)
            : undefined,
        ),
      );
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
  .use("/generation", enforceKey)
  .get("/key", async (c) => c.json(await describeKey(c.var.key)))
  .get("/credits", async (c) =>
    c.json(await describeCredits(c.var.key.organizationId)),
  )
  // any key of the organization may read its requests, as OpenRouter's
  .get("/generation", async (c) => {
    const id = c.req.query("id")?.trim();
    if (!id) {
      return apiError(400, "Missing the id query parameter");
    }
    const generation = await getGeneration(c.var.key.organizationId, id);
    return generation
      ? c.json({ data: generation })
      : apiError(404, "Generation not found");
  });
