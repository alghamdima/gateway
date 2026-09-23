import { Hono } from "hono";

import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";
import {
  getDeleteUrl,
  getPublicUrl,
  getUploadUrl,
  getObjectUrlSchema,
} from "@workspace/storage/server";

import {
  createAvatarPath,
  isOwnedAvatarPath,
  uploadAvatarSchema,
} from "../../lib/avatar";
import { enforceAuth, validate } from "../../middleware";

export const userRouter = new Hono()
  .use(enforceAuth)
  .post("/avatar", validate("json", uploadAvatarSchema), async (c) => {
    const { contentType, contentLength } = c.req.valid("json");
    const path = createAvatarPath({ ownerId: c.var.user.id, contentType });

    const [{ url: uploadUrl }, { url: publicUrl }] = await Promise.all([
      getUploadUrl({ path, contentType, contentLength }),
      getPublicUrl({ path }),
    ]);

    return c.json({ uploadUrl, publicUrl, path });
  })
  .delete("/avatar", validate("query", getObjectUrlSchema), async (c) => {
    const { path } = c.req.valid("query");

    if (!isOwnedAvatarPath({ path, ownerId: c.var.user.id })) {
      throw new HttpException(HttpStatusCode.FORBIDDEN, {
        code: "error.forbidden",
      });
    }

    return c.json(await getDeleteUrl({ path }));
  });
