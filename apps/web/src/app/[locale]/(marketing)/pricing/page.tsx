import { buttonVariants } from "@workspace/ui-web/button";

import { pathsConfig } from "~/config/paths";
import { getTranslation } from "~/lib/i18n";
import { getMetadata } from "~/lib/metadata";
import { TurboLink } from "~/modules/common/turbo-link";
import {
  Section,
  SectionDescription,
  SectionHeader,
  SectionTitle,
} from "~/modules/marketing/layout/section";

export const generateMetadata = getMetadata({
  title: "marketing:pricing.title",
  description: "marketing:pricing.description",
});

const points = ["perModel", "prepaid", "fee"] as const;

export default async function PricingPage() {
  const { t } = await getTranslation({ ns: "marketing" });

  return (
    <Section id="pricing">
      <SectionHeader>
        <SectionTitle as="h1">{t("pricing.title")}</SectionTitle>
        <SectionDescription>{t("pricing.description")}</SectionDescription>
      </SectionHeader>

      <ul className="grid w-full max-w-5xl gap-4 md:grid-cols-3">
        {points.map((point) => (
          <li
            key={point}
            className="bg-card flex flex-col gap-3 rounded-xl border p-6"
          >
            <h2 className="text-lg font-medium tracking-tight">
              {t(`pricing.point.${point}.title`)}
            </h2>
            <p className="text-muted-foreground text-sm text-pretty">
              {t(`pricing.point.${point}.description`)}
            </p>
          </li>
        ))}
      </ul>

      <TurboLink
        href={pathsConfig.marketing.models.index}
        className={buttonVariants()}
      >
        {t("cta.models")}
      </TurboLink>
    </Section>
  );
}
