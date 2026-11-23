import { Hono } from "hono";
import { cors } from "hono/cors";
import { csrf } from "hono/csrf";
import { logger as loggerMiddleware } from "hono/logger";

import { auth } from "@workspace/auth/server";
import { logger } from "@workspace/shared/logger";
import { matchesPattern } from "@workspace/shared/utils";

import { localize, delay } from "./middleware";
import { adminRouter } from "./modules/admin/router";
import { aiRouter } from "./modules/ai/router";
import { authRouter } from "./modules/auth/router";
import { billingRouter } from "./modules/billing/router";
import { creditsRouter } from "./modules/credits/router";
import { organizationRouter } from "./modules/organization/router";
import { userRouter } from "./modules/user/router";
import { monitor } from "./monitor";
import { onError } from "./utils/on-error";

const appRouter = new Hono()
  .basePath("/api")
  .use(
    csrf({
      origin: (origin, c) =>
        [...auth.options.trustedOrigins, new URL(c.req.url).origin].some(
          (trustedOrigin) => matchesPattern(origin, trustedOrigin),
        ),
    }),
  )
  .use(
    cors({
      origin: "*",
      allowHeaders: ["Content-Type", "Authorization"],
      maxAge: 3600,
      credentials: true,
    }),
  )
  .use(loggerMiddleware((...args) => logger.info(...args)))
  .use(delay)
  .use(localize)
  .use(monitor.middleware)
  .route("/status", monitor.routes)
  .route("/admin", adminRouter)
  .route("/ai", aiRouter)
  .route("/auth", authRouter)
  .route("/billing", billingRouter)
  .route("/credits", creditsRouter)
  .route("/organizations", organizationRouter)
  .route("/user", userRouter)
  .onError(onError);

type AppRouter = typeof appRouter;

export type { AppRouter };
export { appRouter };
