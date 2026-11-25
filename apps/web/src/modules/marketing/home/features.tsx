import { Icons } from "@workspace/ui-web/icons";

import { getTranslation } from "~/lib/i18n";
import {
  Section,
  SectionBadge,
  SectionDescription,
  SectionHeader,
  SectionTitle,
} from "~/modules/marketing/layout/section";

const features = [
  { id: "oneApi", icon: Icons.Webhook },
  { id: "credits", icon: Icons.HandCoins },
  { id: "dataPolicy", icon: Icons.Globe2 },
  { id: "evidence", icon: Icons.ShieldUser },
  { id: "keys", icon: Icons.Key },
  { id: "usage", icon: Icons.TrendingUp },
] as const;

export const Features = async () => {
  const { t } = await getTranslation({ ns: "marketing" });

  return (
    <Section id="features">
      <SectionHeader>
        <SectionBadge>{t("features.label")}</SectionBadge>
        <SectionTitle>{t("features.title")}</SectionTitle>
        <SectionDescription>{t("features.description")}</SectionDescription>
      </SectionHeader>

      <ul className="grid w-full max-w-5xl gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {features.map((feature) => (
          <li
            key={feature.id}
            className="bg-card flex flex-col gap-3 rounded-xl border p-6"
          >
            <feature.icon className="text-primary size-6" />
            <h3 className="text-lg font-medium tracking-tight">
              {t(`features.feature.${feature.id}.title`)}
            </h3>
            <p className="text-muted-foreground text-sm text-pretty">
              {t(`features.feature.${feature.id}.description`)}
            </p>
          </li>
        ))}
      </ul>
    </Section>
  );
};
