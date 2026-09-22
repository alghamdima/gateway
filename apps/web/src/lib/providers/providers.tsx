import { NuqsAdapter } from "nuqs/adapters/next/app";

import { I18nProvider, loadLocaleResources } from "@workspace/i18n";

import { appConfig } from "~/config/app";
import { QueryClientProvider } from "~/lib/query/client";

import { AnalyticsProvider } from "./analytics";
import { ConsentProvider } from "./consent";
import { FlagsProvider } from "./flags";
import { MonitoringProvider } from "./monitoring";
import { MotionProvider } from "./motion";
import { ThemeProvider } from "./theme";

interface ProvidersProps {
  readonly children: React.ReactNode;
  readonly locale: string;
}

export const Providers = async ({ children, locale }: ProvidersProps) => {
  const resources = await loadLocaleResources(locale);

  return (
    <I18nProvider
      locale={locale}
      defaultLocale={appConfig.locale}
      resources={resources}
    >
      <QueryClientProvider>
        <NuqsAdapter>
          <ConsentProvider locale={locale}>
            <AnalyticsProvider>
              <MonitoringProvider>
                <FlagsProvider>
                  <ThemeProvider>
                    <MotionProvider>{children}</MotionProvider>
                  </ThemeProvider>
                </FlagsProvider>
              </MonitoringProvider>
            </AnalyticsProvider>
          </ConsentProvider>
        </NuqsAdapter>
      </QueryClientProvider>
    </I18nProvider>
  );
};
