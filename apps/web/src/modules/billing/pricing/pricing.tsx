"use client";

import { getFilteredPlans } from "@workspace/billing";
import { Skeleton } from "@workspace/ui-web/skeleton";

import { usePricingControls } from "~/modules/billing/pricing/controls";
import { Section, SectionHeader } from "~/modules/marketing/layout/section";

import { PricingHeader } from "./layout/header";
import { Plans, PlansSkeleton } from "./plans/plans";

export const Pricing = () => {
  const controls = usePricingControls((c) => c.controls);

  const filters = {
    ...controls,
    hidden: false,
  };
  const filteredPlans = getFilteredPlans(filters);

  return (
    <Section id="pricing" className="gap-10 sm:gap-12 md:gap-16 lg:gap-20">
      <PricingHeader />
      <Plans plans={filteredPlans} />
    </Section>
  );
};

export const PricingSkeleton = () => {
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
