import * as React from "react";
import { Heading, Preview, Text } from "react-email";

import { getTranslation } from "@workspace/i18n/server";
import { getOrigin } from "@workspace/shared/utils";

import { Button } from "./_components/button";
import { Layout } from "./_components/layout/layout";

import type { CommonEmailProps, EmailTemplate, EmailVariables } from "../types";

type Props = EmailVariables[typeof EmailTemplate.RECONCILIATION_ALERT] &
  CommonEmailProps;

/** To staff: the daily comparison of gateway and ledger found something (step 10.1). */
export const ReconciliationAlert = async ({
  url,
  period,
  status,
  organizations,
  reapedHolds,
  unattributed,
  locale,
}: Props) => {
  const { t } = await getTranslation({ locale, ns: "admin" });

  return (
    <Layout origin={getOrigin(url)} locale={locale}>
      <Preview>{t("reconciliation.email.preview", { period })}</Preview>
      <Heading className="leading-tight tracking-tight">
        {t("reconciliation.email.subject")}
      </Heading>
      <Text>
        {status === "error"
          ? t("reconciliation.email.error", { period })
          : t("reconciliation.email.mismatch", { period })}
      </Text>
      <Text>
        {t("reconciliation.email.counts", {
          organizations,
          reapedHolds,
          unattributed,
        })}
      </Text>
      <Button href={url}>{t("reconciliation.email.cta")}</Button>
    </Layout>
  );
};

ReconciliationAlert.subject = async ({ locale }: CommonEmailProps) => {
  const { t } = await getTranslation({ locale, ns: "admin" });
  return t("reconciliation.email.subject");
};

ReconciliationAlert.PreviewProps = {
  url: "https://example.com/admin/reconciliation",
  period: "2026-11-23",
  status: "mismatch",
  organizations: 1,
  reapedHolds: 2,
  unattributed: 0,
};

export default ReconciliationAlert;
