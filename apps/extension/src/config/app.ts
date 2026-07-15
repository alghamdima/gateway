import { ThemeFont, ThemeRadius } from "@workspace/ui";

import env from "../../env.config";

export const appConfig = {
  name: env.VITE_PRODUCT_NAME,
  url: env.VITE_SITE_URL,
  locale: env.VITE_DEFAULT_LOCALE,
  theme: {
    mode: env.VITE_THEME_MODE,
    color: env.VITE_THEME_COLOR,
    radius: ThemeRadius.MEDIUM,
    font: ThemeFont.SANS,
  },
} as const;
