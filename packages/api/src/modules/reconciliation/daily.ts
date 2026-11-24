import { EmailTemplate } from "@workspace/email";
import { sendEmail } from "@workspace/email/server";
import { logger } from "@workspace/shared/logger";

import { env } from "../../env";
import { previousDay, reconcile } from "./reconcile";

import type { GatewayClient } from "@workspace/gateway/server";

/**
 * Reconciles the UTC day before `now` and, when it finds something or cannot
 * run, emails RECONCILIATION_ALERT_EMAIL (step 10.1). Run once a day by
 * whatever scheduler the deployment has, through POST /api/internal/reconciliation.
 */
export const runDailyReconciliation = async ({
  now = new Date(),
  origin,
  gateway,
  alertTo = env.RECONCILIATION_ALERT_EMAIL,
}: {
  now?: Date;
  /** The site's origin, for the link in the alert. */
  origin: string;
  gateway?: GatewayClient;
  alertTo?: string;
}) => {
  const period = previousDay(now);
  const result = await reconcile(period, gateway);
  const day = period.from.toISOString().slice(0, 10);

  if (result.status === "ok") {
    logger.info(`Reconciliation ${day}: gateway and ledger agree`);
    return { ...result, alerted: false };
  }
  logger.error(`Reconciliation ${day}: ${result.status}`, result.findings);
  if (!alertTo) {
    logger.warn("RECONCILIATION_ALERT_EMAIL is not set: no one was emailed");
    return { ...result, alerted: false };
  }
  await sendEmail({
    to: alertTo,
    template: EmailTemplate.RECONCILIATION_ALERT,
    variables: {
      url: new URL("/admin/reconciliation", origin).toString(),
      period: day,
      status: result.status,
      organizations: result.findings.organizations.length,
      reapedHolds: result.findings.reapedHolds.reduce(
        (sum, r) => sum + r.holds,
        0,
      ),
      unattributed: result.findings.unattributed.length,
    },
  });
  return { ...result, alerted: true };
};
