import { connection } from "next/server";
import { cache } from "react";

import { handle } from "@workspace/api/utils";
import { logger } from "@workspace/shared/logger";

import { api } from "~/lib/api/server";

export interface SiteSettings {
  isBlogEnabled: boolean;
}

/**
 * What staff have switched on the public site (SITE-01), read once per
 * request. If it cannot be read, the optional parts are treated as off: a
 * page staff switched off must not come back because of an error.
 */
export const getSiteSettings = cache(async (): Promise<SiteSettings> => {
  // read per request, never at build time: the catch below must not swallow
  // the signal Next.js throws to leave a prerender
  await connection();
  try {
    return await handle(api.site.settings.$get)();
  } catch (error) {
    logger.error(error);
    return { isBlogEnabled: false };
  }
});
