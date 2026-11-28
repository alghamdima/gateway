"use client";

import { useTranslation } from "@workspace/i18n";
import { Icons } from "@workspace/ui-web/icons";

import { pathsConfig } from "~/config/paths";
import { ThemeControls } from "~/modules/common/theme";
import { TurboLink } from "~/modules/common/turbo-link";
import { CtaButton } from "~/modules/marketing/layout/cta-button";

import { MobileNavigation } from "./navigation/mobile-navigation";
import { Navigation } from "./navigation/navigation";

import type { Session } from "~/lib/auth/client";
import type { SiteSettings } from "~/modules/marketing/site";

const allLinks = [
  {
    label: "resources",
    items: [
      {
        title: "marketing:contact.label",
        description: "marketing:contact.description",
        href: pathsConfig.marketing.contact,
        icon: Icons.SendHorizontal,
      },
    ],
  },
  {
    label: "marketing:models.label",
    href: pathsConfig.marketing.models.index,
  },
  {
    label: "billing:pricing.label",
    href: pathsConfig.marketing.pricing,
  },
  {
    label: "marketing:blog.label",
    href: pathsConfig.marketing.blog.index,
  },
] as const;

export const Header = ({
  initialSession,
  site,
}: {
  initialSession: Session | null;
  site: SiteSettings;
}) => {
  const { t } = useTranslation("common");
  // the blog is linked only while staff have it switched on (SITE-01)
  const links = allLinks.filter(
    (link) =>
      site.isBlogEnabled ||
      !("href" in link) ||
      link.href !== pathsConfig.marketing.blog.index,
  );
  return (
    <header className="bg-background/80 sticky inset-0 top-(--banner-height,0px) z-40 w-full py-3 backdrop-blur-sm">
      <div className="flex items-center justify-between px-6 pr-4 sm:container">
        <TurboLink
          href={pathsConfig.index}
          className="flex shrink-0 items-center gap-3"
          aria-label={t("home")}
        >
          <Icons.Logo className="text-primary h-8" />
          <Icons.LogoText className="text-foreground h-4" />
        </TurboLink>

        <Navigation links={links} />

        <div className="flex items-center justify-center lg:gap-2">
          <ThemeControls />
          <CtaButton
            className="hidden lg:inline-flex"
            initialSession={initialSession}
          />
          <MobileNavigation links={links} initialSession={initialSession} />
        </div>
      </div>
    </header>
  );
};
