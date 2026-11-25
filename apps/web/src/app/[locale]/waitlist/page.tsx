import { Icons } from "@workspace/ui-web/icons";

import { pathsConfig } from "~/config/paths";
import { getTranslation } from "~/lib/i18n";
import { getMetadata } from "~/lib/metadata";
import { TurboLink } from "~/modules/common/turbo-link";
import { WaitlistForm } from "~/modules/marketing/waitlist/form";
import { WaitlistGradient } from "~/modules/marketing/waitlist/gradient";

export const generateMetadata = getMetadata({
  title: "marketing:waitlist.label",
  description: "marketing:waitlist.description",
});

export default async function WaitlistPage() {
  const { t } = await getTranslation({ ns: ["marketing", "common"] });

  return (
    <main className="bg-background relative flex min-h-svh flex-col items-center justify-between overflow-hidden p-8 md:p-10 lg:p-12">
      <WaitlistGradient />

      <TurboLink
        href={pathsConfig.index}
        aria-label={t("common:home")}
        className="relative z-10"
      >
        <Icons.Logo className="text-primary h-8 md:h-10" />
      </TurboLink>

      <div className="relative z-10 flex w-full max-w-3xl flex-col items-center gap-10 text-center">
        <div className="flex flex-col items-center gap-4">
          <h1 className="text-foreground animate-fade-in -translate-y-4 text-4xl font-bold tracking-tight text-balance sm:text-5xl md:text-6xl lg:text-7xl">
            {t("waitlist.title")}
          </h1>
          <p className="text-muted-foreground animate-fade-in max-w-xl -translate-y-4 text-base text-balance opacity-0 [--animation-delay:100ms] sm:text-lg lg:text-xl">
            {t("waitlist.subtitle")}
          </p>
        </div>

        <WaitlistForm />
      </div>

      <div aria-hidden className="h-8" />
    </main>
  );
}
