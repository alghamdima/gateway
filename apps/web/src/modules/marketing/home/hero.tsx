import { buttonVariants } from "@workspace/ui-web/button";

import { pathsConfig } from "~/config/paths";
import { getSession } from "~/lib/auth/server";
import { getTranslation } from "~/lib/i18n";
import { TurboLink } from "~/modules/common/turbo-link";
import { CtaButton } from "~/modules/marketing/layout/cta-button";
import {
  Section,
  SectionDescription,
  SectionHeader,
  SectionTitle,
} from "~/modules/marketing/layout/section";

export const Hero = async () => {
  const { t } = await getTranslation({ ns: "marketing" });
  const session = await getSession();

  return (
    <Section id="hero" className="gap-6 sm:gap-6 md:gap-6 lg:gap-6">
      <SectionHeader>
        <SectionTitle as="h1">{t("product.title")}</SectionTitle>
        <SectionDescription>{t("product.description")}</SectionDescription>
      </SectionHeader>

      <div className="flex w-full flex-col gap-2 sm:w-auto sm:flex-row sm:gap-4">
        <CtaButton initialSession={session} />

        <TurboLink
          href={pathsConfig.marketing.models.index}
          className={buttonVariants({ variant: "outline" })}
        >
          {t("cta.models")}
        </TurboLink>
      </div>
    </Section>
  );
};
