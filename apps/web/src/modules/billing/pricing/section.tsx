"use client";

import { memo, useState } from "react";

import {
  config,
  BillingModel,
  RecurringInterval,
  getFilteredPlans,
} from "@workspace/billing";
import { Skeleton } from "@workspace/ui-web/skeleton";

import { Section, SectionHeader } from "~/modules/marketing/layout/section";

import { PricingHeader } from "./layout/header";
import { Plans, PlansSkeleton } from "./plans/plans";

import type { User } from "@workspace/auth";

interface PricingSectionProps {
  readonly user: User | null;
}

export const PricingSection = memo<PricingSectionProps>(({ user }) => {
  const intervals = [
    ...new Set(
      config.plans.flatMap((plan) =>
        plan.variants
          .flatMap((variant) =>
            "interval" in variant ? variant.interval : null,
          )
          .filter(Boolean),
      ),
    ),
  ];

  const [activeInterval, setActiveInterval] = useState<RecurringInterval>(
    intervals[0] ?? RecurringInterval.MONTH,
  );

  const filters = {
    model: BillingModel.RECURRING,
    interval: activeInterval,
    hidden: false,
  };
  const filteredPlans = getFilteredPlans(filters);

  return (
    <Section id="pricing" className="gap-10 sm:gap-12 md:gap-16 lg:gap-20">
      <PricingHeader
        intervals={intervals}
        model={BillingModel.RECURRING}
        activeInterval={activeInterval}
        onIntervalChange={setActiveInterval}
      />
      <Plans plans={filteredPlans} user={user} />
    </Section>
  );
});

export const PricingSectionSkeleton = () => {
  return (
    <Section id="pricing" className="gap-10 sm:gap-12 md:gap-16 lg:gap-20">
      <SectionHeader className="flex flex-col items-center justify-center gap-3">
        <Skeleton className="h-8 w-32" />
        <Skeleton className="mt-4 h-12 w-72" />
        <Skeleton className="h-8 w-96" />
      </SectionHeader>
      <PlansSkeleton />
    </Section>
  );
};

PricingSection.displayName = "PricingSection";
