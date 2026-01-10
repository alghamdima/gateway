"use client";

import { memo } from "react";

import { BillingModel } from "@workspace/billing";
import { useTranslation } from "@workspace/i18n";
import { Tabs, TabsList, TabsTrigger } from "@workspace/ui-web/tabs";

import {
  SectionBadge,
  SectionDescription,
  SectionHeader,
  SectionTitle,
} from "~/modules/marketing/layout/section";

import { Discount } from "./discount";

import type { RecurringInterval } from "@workspace/billing";

interface PricingHeaderProps {
  readonly model: BillingModel;
  readonly intervals: RecurringInterval[];
  readonly activeInterval: RecurringInterval;
  readonly onIntervalChange: (billing: RecurringInterval) => void;
}

export const PricingHeader = memo<PricingHeaderProps>(
  ({ model, activeInterval, intervals, onIntervalChange }) => {
    const { t } = useTranslation(["common", "billing"]);

    return (
      <SectionHeader>
        <SectionBadge>{t("pricing.label")}</SectionBadge>
        <SectionTitle>{t("pricing.title")}</SectionTitle>
        <SectionDescription className="text-muted-foreground max-w-2xl text-center">
          {t("pricing.description")}
        </SectionDescription>

        <Discount />

        {model === BillingModel.RECURRING && intervals.length > 0 && (
          <Tabs
            className="mt-2 lg:mt-4"
            value={activeInterval}
            onValueChange={(value) =>
              onIntervalChange(value as RecurringInterval)
            }
          >
            <TabsList>
              {intervals.map((interval) => (
                <TabsTrigger
                  key={interval}
                  value={interval}
                  className="capitalize"
                  aria-controls={undefined}
                >
                  {t(interval)}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
        )}
      </SectionHeader>
    );
  },
);

PricingHeader.displayName = "PricingHeader";
