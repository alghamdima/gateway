"use client";

import { ConsentDialogLink } from "@c15t/nextjs";

import { useTranslation } from "@workspace/i18n";
import { isExternal } from "@workspace/shared/utils";
import { Icons } from "@workspace/ui-web/components/icons";

import { pathsConfig } from "~/config/paths";
import { TurboLink } from "~/modules/common/turbo-link";

import type { SiteSettings } from "~/modules/marketing/site";

const links = [
  {
    label: "resources",
    items: [
      {
        title: "marketing:contact.label",
        href: pathsConfig.marketing.contact,
      },
      {
        title: "marketing:models.label",
        href: pathsConfig.marketing.models.index,
      },
    ],
  },
  {
    label: "about",
    items: [
      {
        title: "billing:pricing.label",
        href: pathsConfig.marketing.pricing,
      },
      {
        title: "marketing:blog.label",
        href: pathsConfig.marketing.blog.index,
      },
    ],
  },
  {
    label: "legal.label",
    items: [
      {
        title: "legal.privacy",
        href: pathsConfig.marketing.legal("privacy-policy"),
      },
      {
        title: "legal.cookiePolicy",
        href: pathsConfig.marketing.legal("cookie-policy"),
      },
      {
        title: "legal.terms",
        href: pathsConfig.marketing.legal("terms-and-conditions"),
      },
      {
        title: "legal.cookies",
        component: ConsentDialogLink,
      },
    ],
  },
] as const;

export const FooterNavigation = ({ site }: { site: SiteSettings }) => {
  const { t } = useTranslation(["common", "marketing", "billing"]);
  // the blog is linked only while staff have it switched on (SITE-01)
  const isHidden = (href: string) =>
    !site.isBlogEnabled && href === pathsConfig.marketing.blog.index;

  return (
    <div className="mt-1 grid w-full max-w-200 grid-cols-2 gap-8 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-3 xl:grid-cols-4">
      {links.map((link) => (
        <div className="flex w-full flex-col gap-5" key={link.label}>
          <span className="text-foreground text-sm font-medium">
            {t(link.label)}
          </span>
          <nav>
            <ul className="flex flex-col gap-3">
              {link.items.map((item) =>
                "href" in item && isHidden(item.href) ? null : (
                  <li
                    key={item.title}
                    className="text-muted-foreground hover:text-foreground relative text-sm transition-colors"
                  >
                    {"component" in item ? (
                      <item.component className="cursor-pointer">
                        {t(item.title)}
                      </item.component>
                    ) : (
                      <TurboLink href={item.href}>
                        {t(item.title)}
                        {isExternal(item.href) && (
                          <Icons.ArrowUpRight className="-mt-1 inline size-2.5" />
                        )}
                      </TurboLink>
                    )}
                  </li>
                ),
              )}
            </ul>
          </nav>
        </div>
      ))}
    </div>
  );
};
