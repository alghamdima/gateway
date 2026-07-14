import { ThemeRadius } from "@workspace/ui";

import env from "../../env.config";

export const appConfig = {
  locale: env.EXPO_PUBLIC_DEFAULT_LOCALE,
  url: env.EXPO_PUBLIC_SITE_URL,
  theme: {
    mode: env.EXPO_PUBLIC_THEME_MODE,
    color: env.EXPO_PUBLIC_THEME_COLOR,
    radius: ThemeRadius.MEDIUM,
  },
} as const;
