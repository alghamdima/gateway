"use client";

import {
  ConsentBanner,
  ConsentDialog,
  ConsentManagerProvider,
  policyPackPresets,
  Translations,
} from "@c15t/nextjs";
import { baseTranslations } from "@c15t/translations/all";
import { memo } from "react";

import { Locale } from "@workspace/i18n";

import { pathsConfig } from "~/config/paths";

import env from "../../../env.config";

interface ConsentProviderProps {
  readonly children: React.ReactNode;
  readonly locale: string;
}

export const ConsentProvider = memo<ConsentProviderProps>(
  ({ children, locale }) => {
    return (
      <ConsentManagerProvider
        options={{
          ...(env.NEXT_PUBLIC_C15T_URL
            ? { mode: "hosted" as const, backendURL: env.NEXT_PUBLIC_C15T_URL }
            : {
                mode: "offline" as const,
                offlinePolicy: {
                  policyPacks: [
                    policyPackPresets.europeOptIn(),
                    policyPackPresets.californiaOptOut(),
                    policyPackPresets.worldNoBanner(),
                  ],
                },
              }),
          consentCategories: ["necessary", "measurement"],
          legalLinks: {
            privacyPolicy: {
              href: pathsConfig.marketing.legal("privacy-policy"),
              target: "_self",
            },
            cookiePolicy: {
              href: pathsConfig.marketing.legal("cookie-policy"),
              target: "_self",
            },
          },
          i18n: {
            locale,
            detectBrowserLanguage: false,
            messages: {
              [Locale.EN]: baseTranslations.en,
              [Locale.ES]: baseTranslations.es,
            } satisfies Record<Locale, Partial<Translations>>,
          },
          theme: {
            consentActions: {
              customize: { variant: "neutral", mode: "ghost" },
              reject: { variant: "neutral", mode: "stroke" },
              accept: { variant: "primary", mode: "filled" },
            },
            slots: {
              consentBanner: {
                style: {
                  maxWidth: "calc(var(--consent-banner-max-width) + 3rem)",
                },
              },
            },
          },
        }}
      >
        <ConsentBanner
          hideBranding
          legalLinks={["privacyPolicy", "cookiePolicy"]}
          layout={["customize", ["reject", "accept"]]}
        />
        <ConsentDialog
          hideBranding
          legalLinks={["privacyPolicy", "cookiePolicy"]}
        />
        {children}
      </ConsentManagerProvider>
    );
  },
);

ConsentProvider.displayName = "ConsentProvider";
