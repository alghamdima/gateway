import * as React from "react";
import { Heading, Preview, Text } from "react-email";

import { getTranslation } from "@workspace/i18n/server";
import { getOrigin } from "@workspace/shared/utils";

import { Button } from "./_components/button";
import { Layout } from "./_components/layout/layout";

import type { CommonEmailProps, EmailTemplate, EmailVariables } from "../types";

type Props = EmailVariables[typeof EmailTemplate.LOW_BALANCE] &
  CommonEmailProps;

/** To an organization's owners and admins: its credits are running out (step 10.2). */
export const LowBalance = async ({
  url,
  organization,
  available,
  threshold,
  locale,
}: Props) => {
  const { t } = await getTranslation({ locale, ns: "organization" });

  return (
    <Layout origin={getOrigin(url)} locale={locale}>
      <Preview>
        {t("credits.lowBalance.email.preview", { organization, available })}
      </Preview>
      <Heading className="leading-tight tracking-tight">
        {t("credits.lowBalance.email.subject", { organization })}
      </Heading>
      <Text>
        {t("credits.lowBalance.email.body", {
          organization,
          available,
          threshold,
        })}
      </Text>
      <Button href={url}>{t("credits.lowBalance.email.cta")}</Button>
    </Layout>
  );
};

LowBalance.subject = async ({ locale }: CommonEmailProps) => {
  const { t } = await getTranslation({ locale, ns: "organization" });
  return t("credits.lowBalance.email.subjectShort");
};

LowBalance.PreviewProps = {
  url: "https://example.com/dashboard/acme/credits",
  organization: "Acme",
  available: "$1.20",
  threshold: "$5.00",
};

export default LowBalance;
