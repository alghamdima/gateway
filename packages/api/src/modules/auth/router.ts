import { Hono } from "hono";

import { ERROR_MESSAGES } from "@workspace/auth";
import { auth } from "@workspace/auth/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException, isHttpStatus } from "@workspace/shared/utils";

import type { AuthErrorCode } from "@workspace/auth";

export const authRouter = new Hono().on(["GET", "POST"], "*", async (c) => {
  const res = await auth.handler(c.req.raw);

  if (["2", "3"].includes(res.status.toString().slice(0, 1))) {
    return res;
  }

  const json = (await res.json()) as { code: AuthErrorCode; message: string };

  throw new HttpException(
    isHttpStatus(res.status)
      ? res.status
      : HttpStatusCode.INTERNAL_SERVER_ERROR,
    {
      code: ERROR_MESSAGES[json.code],
    },
  );
});
