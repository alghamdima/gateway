import { useQuery } from "@tanstack/react-query";
import { memo } from "react";

import {
  BillingModel,
  calculateDiscount,
  calculateRecurringDiscount,
  formatPrice,
  getActivePlan,
  getLowerPlans,
  getVariantDiscount,
} from "@workspace/billing";
import { isKey, useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import { Badge } from "@workspace/ui-web/badge";
import { Button, buttonVariants } from "@workspace/ui-web/button";
import { Card } from "@workspace/ui-web/card";
import { Icons } from "@workspace/ui-web/icons";

import { pathsConfig } from "~/config/paths";
import { billing } from "~/modules/billing/lib/api";
import { TurboLink } from "~/modules/common/turbo-link";

import { usePlan } from "./hooks/use-plan";

import type { User } from "@workspace/auth";
import type {
  BillingConfigPlan,
  BillingConfigVariant,
  BillingConfigDiscount,
} from "@workspace/billing";

const Price = ({
  variant,
  discount,
}: {
  variant: BillingConfigVariant;
  discount?: BillingConfigDiscount;
}) => {
  const { t, i18n } = useTranslation(["common", "billing"]);

  const priceWithDiscount = discount
    ? calculateDiscount(variant, discount)
    : variant.model === BillingModel.RECURRING
      ? calculateRecurringDiscount(variant)
      : null;

  const renderInterval = () =>
    !variant.custom && (
      <span className="text-muted-foreground shrink-0 text-lg">
        /{" "}
        {variant.model === BillingModel.RECURRING
          ? t(variant.interval)
          : t("lifetime")}
      </span>
    );

  if (priceWithDiscount) {
    return (
      <p className="relative flex w-fit items-end gap-1 py-2">
        <span className="text-muted-foreground mr-2 text-lg line-through md:text-xl">
          {formatPrice(
            {
              amount: priceWithDiscount.original.cost,
              currency: priceWithDiscount.original.currency,
            },
            i18n.language,
          )}
        </span>
        <span className="text-4xl font-bold tracking-tighter md:text-5xl">
          {formatPrice(
            {
              amount: priceWithDiscount.discounted.cost,
              currency: priceWithDiscount.discounted.currency,
            },
            i18n.language,
          )}
        </span>
        {renderInterval()}
      </p>
    );
  }

  return (
    <p className="relative flex items-end gap-1 py-2">
      <span className="text-4xl font-bold tracking-tighter md:text-5xl">
        {variant.custom
          ? isKey(variant.label, i18n, "billing")
            ? t(variant.label)
            : variant.label
          : formatPrice(
              {
                amount: variant.cost,
                currency: variant.currency,
              },
              i18n.language,
            )}
      </span>
      {renderInterval()}
    </p>
  );
};

interface PlanProps {
  readonly plan: BillingConfigPlan;
  readonly user: User | null;
}

export const Plan = memo<PlanProps>(({ plan, user }) => {
  const { t, i18n } = useTranslation(["common", "billing"]);

  const summary = useQuery({ ...billing.queries.summary.get, enabled: !!user });
  const activePlan = getActivePlan(summary.data);

  const { features, isPending, handleCheckout, handleOpenPortal } =
    usePlan(plan);

  const hasPlan = !getLowerPlans(plan.id).some((p) => p.id === activePlan);

  return plan.variants.map((variant) => {
    const discount = getVariantDiscount(variant);

    return (
      <div className="grow basis-[350px] rounded-lg" key={variant.id}>
        <Card
          className={cn(
            "relative flex h-full flex-col gap-6 px-7 py-6 md:p-8",
            plan.badge && "border-primary",
          )}
        >
          {plan.badge && (
            <Badge className="hover:bg-primary absolute top-0 left-1/2 -translate-x-1/2 -translate-y-1/2 px-4 py-1.5 uppercase">
              {isKey(plan.badge, i18n, "billing") ? t(plan.badge) : plan.badge}
            </Badge>
          )}

          <div>
            <span className="text-lg font-semibold">
              {isKey(plan.name, i18n, "billing") ? t(plan.name) : plan.name}
            </span>

            <Price variant={variant} discount={discount} />

            <p className="text-sm">
              {isKey(plan.description, i18n, "billing")
                ? t(plan.description)
                : plan.description}
            </p>
          </div>

          <div className="flex flex-col gap-1">
            {features.map((feature) => (
              <div
                key={feature.id}
                className={cn("flex items-center gap-3 py-1", {
                  "opacity-50": !feature.available,
                })}
              >
                <div
                  className={cn(
                    "flex size-5 shrink-0 items-center justify-center rounded-full",
                    feature.available ? "bg-primary" : "border-primary border",
                  )}
                >
                  {feature.available ? (
                    <Icons.CheckIcon className="text-primary-foreground w-3" />
                  ) : (
                    <Icons.X className="text-primary w-3" />
                  )}
                </div>
                <span className="text-md">
                  {isKey(feature.key, i18n, "billing")
                    ? t(feature.key)
                    : feature.key}
                </span>
              </div>
            ))}
          </div>

          <div className="mt-auto flex flex-col gap-2">
            {"trialDays" in variant && variant.trialDays && !hasPlan && (
              <Button
                variant="outline"
                onClick={() =>
                  handleCheckout({
                    user,
                    variant: { id: variant.id },
                    ...(discount ? { discount: { code: discount.code } } : {}),
                  })
                }
                disabled={isPending}
              >
                {isPending ? (
                  <Icons.Loader2 className="animate-spin" />
                ) : (
                  t("trial.period", { period: variant.trialDays })
                )}
              </Button>
            )}

            {variant.custom ? (
              <TurboLink href={variant.href} className={buttonVariants()}>
                {hasPlan ? t("manage") : t("getStarted")}
              </TurboLink>
            ) : variant.cost === 0 ? (
              <TurboLink
                href={
                  user
                    ? pathsConfig.dashboard.user.index
                    : pathsConfig.auth.login
                }
                className={buttonVariants({ variant: "outline" })}
              >
                {user ? t("goToDashboard") : t("trial.cta")}
              </TurboLink>
            ) : (
              <Button
                onClick={() =>
                  variant.model === BillingModel.RECURRING && hasPlan
                    ? handleOpenPortal({ user })
                    : handleCheckout({
                        user,
                        variant: { id: variant.id },
                        ...(discount
                          ? { discount: { code: discount.code } }
                          : {}),
                      })
                }
                disabled={isPending}
              >
                {isPending ? (
                  <Icons.Loader2 className="animate-spin" />
                ) : variant.model === BillingModel.RECURRING && hasPlan ? (
                  t("manage")
                ) : variant.model === BillingModel.RECURRING ? (
                  t("subscribe")
                ) : (
                  t("getLifetimeAccess")
                )}
              </Button>
            )}
          </div>
        </Card>
      </div>
    );
  });
});

Plan.displayName = "Plan";
