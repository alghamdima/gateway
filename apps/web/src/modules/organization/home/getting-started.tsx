import { buttonVariants } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";

import { pathsConfig } from "~/config/paths";
import { getTranslation } from "~/lib/i18n";
import { TurboLink } from "~/modules/common/turbo-link";

interface GettingStartedProps {
  /** The organization's slug, for the links. */
  readonly organization: string;
}

/**
 * The organization's home: the three steps to a first API call, and where the
 * data policy is. Links only; each page shows its own live data.
 */
export const GettingStarted = async ({ organization }: GettingStartedProps) => {
  const { t } = await getTranslation({ ns: "dashboard" });
  const paths = pathsConfig.dashboard.organization(organization);

  const steps = [
    { id: "credits", href: paths.credits },
    { id: "key", href: paths.keys },
    { id: "call", href: pathsConfig.marketing.models.index },
  ] as const;

  return (
    <div className="flex w-full flex-col gap-4">
      <header className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold tracking-tight">
          {t("organization.home.title")}
        </h1>
        <p className="text-muted-foreground text-sm">
          {t("organization.home.description")}
        </p>
      </header>

      <ol className="grid gap-4 lg:grid-cols-3">
        {steps.map((step, index) => (
          <li key={step.id}>
            <Card className="h-full">
              <CardHeader>
                <CardDescription>{index + 1}</CardDescription>
                <CardTitle>
                  {t(`organization.home.steps.${step.id}.title`)}
                </CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col items-start gap-4">
                <p className="text-muted-foreground text-sm text-pretty">
                  {t(`organization.home.steps.${step.id}.description`)}
                </p>
                <TurboLink
                  href={step.href}
                  className={buttonVariants({ variant: "outline", size: "sm" })}
                >
                  {t(`organization.home.steps.${step.id}.action`)}
                </TurboLink>
              </CardContent>
            </Card>
          </li>
        ))}
      </ol>

      <Card>
        <CardHeader>
          <CardTitle>{t("organization.home.dataPolicy.title")}</CardTitle>
          <CardDescription>
            {t("organization.home.dataPolicy.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <TurboLink
            href={paths.dataPolicy}
            className={buttonVariants({ variant: "outline", size: "sm" })}
          >
            {t("organization.home.dataPolicy.action")}
          </TurboLink>
        </CardContent>
      </Card>
    </div>
  );
};
