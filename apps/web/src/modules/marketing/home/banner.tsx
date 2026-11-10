import { cn } from "@workspace/ui";
import { buttonVariants } from "@workspace/ui-web/button";

import { getTranslation } from "~/lib/i18n";
import { Section } from "~/modules/marketing/layout/section";

export const Banner = async () => {
  const { t } = await getTranslation({ ns: "marketing" });
  return (
    <Section
      id="banner"
      className="bg-primary text-primary-foreground max-w-full! gap-4 sm:gap-6 md:gap-8 lg:gap-10"
    >
      <h3 className="text-3xl leading-[0.95] font-semibold tracking-tighter text-balance md:text-4xl lg:text-5xl">
        {t("cta.question")}
      </h3>

      <a
        href="https://turbostarter.lemonsqueezy.com/buy/b4a3d6cd-bf86-4cf0-af3f-78fa10f9636c?enabled=542201"
        target="_blank"
        rel="noopener noreferrer"
        className={cn(buttonVariants({ variant: "secondary" }))}
      >
        {t("cta.buy.button")}
      </a>
    </Section>
  );
};
