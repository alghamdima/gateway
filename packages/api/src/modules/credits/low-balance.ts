import { eq, sql } from "@workspace/db";
import { lowBalanceAlert } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { EmailTemplate } from "@workspace/email";
import { sendEmail } from "@workspace/email/server";
import { logger } from "@workspace/shared/logger";
import { formatMicroSar } from "@workspace/shared/utils";

import { MICRO_SAR } from "./ledger";

/**
 * Emails an organization's owners and admins when its spendable credits drop
 * below its threshold (step 10.2): once per dip, again only after the balance
 * has been back above it. Run hourly through GET /api/internal/low-balance.
 */

/** SAR 20 (it was $5, × 3.75 rounded); staff set it in the admin with ADMIN-14. */
export const DEFAULT_LOW_BALANCE_THRESHOLD = 20 * MICRO_SAR;

export interface LowBalanceSetting {
  /** micro-SAR; null when the organization turned the alert off. */
  thresholdMicroSar: number | null;
}

export const getLowBalanceSetting = async (
  organizationId: string,
): Promise<LowBalanceSetting> => {
  const [row] = await db
    .select({ thresholdMicroSar: lowBalanceAlert.thresholdMicroSar })
    .from(lowBalanceAlert)
    .where(eq(lowBalanceAlert.organizationId, organizationId));
  return row ?? { thresholdMicroSar: DEFAULT_LOW_BALANCE_THRESHOLD };
};

/** A new threshold starts a new watch: the next dip below it is emailed. */
export const setLowBalanceSetting = async (
  organizationId: string,
  thresholdMicroSar: number | null,
): Promise<LowBalanceSetting> => {
  await db
    .insert(lowBalanceAlert)
    .values({ organizationId, thresholdMicroSar, notifiedAt: null })
    .onConflictDoUpdate({
      target: lowBalanceAlert.organizationId,
      set: { thresholdMicroSar, notifiedAt: null },
    });
  return { thresholdMicroSar };
};

// a row of the raw query below: a type, because the driver wants an index signature
type Candidate = {
  organization_id: string;
  name: string;
  slug: string;
  available: string;
  threshold: string;
  notified: boolean;
};

const sar = (microSar: number) =>
  formatMicroSar(microSar, "en", { maximumFractionDigits: 6 });

export const checkLowBalances = async ({
  origin,
  now = new Date(),
}: {
  origin: string;
  now?: Date;
}) => {
  // organizations that have had credit at least once (a new one sits at zero
  // and is not "running low"), with the alert on
  const candidates = [
    ...(await db.execute<Candidate>(sql`
      SELECT o.id AS organization_id, o.name, o.slug,
             (a.balance - a.held) AS available,
             coalesce(l.threshold_micro_sar, ${DEFAULT_LOW_BALANCE_THRESHOLD}) AS threshold,
             (l.notified_at IS NOT NULL) AS notified
        FROM credit_account a
        JOIN organization o ON o.id = a.organization_id
        LEFT JOIN low_balance_alert l ON l.organization_id = a.organization_id
       WHERE (l.organization_id IS NULL OR l.threshold_micro_sar IS NOT NULL)
         AND EXISTS (
           SELECT 1 FROM credit_ledger_entry e
            WHERE e.organization_id = a.organization_id AND e.amount > 0
         )`)),
  ];

  let emailed = 0;
  let cleared = 0;
  for (const org of candidates) {
    const available = Number(org.available);
    const threshold = Number(org.threshold);

    if (available >= threshold) {
      if (org.notified) {
        await db
          .update(lowBalanceAlert)
          .set({ notifiedAt: null })
          .where(eq(lowBalanceAlert.organizationId, org.organization_id));
        cleared++;
      }
      continue;
    }
    if (org.notified) {
      continue;
    }

    // mark first, so a failure while emailing does not send twice next hour
    await db
      .insert(lowBalanceAlert)
      .values({
        organizationId: org.organization_id,
        thresholdMicroSar: threshold,
        notifiedAt: now,
      })
      .onConflictDoUpdate({
        target: lowBalanceAlert.organizationId,
        set: { notifiedAt: now },
      });

    const recipients = [
      ...(await db.execute<{ email: string }>(sql`
        SELECT u.email FROM member m JOIN "user" u ON u.id = m.user_id
         WHERE m.organization_id = ${org.organization_id}
           AND (m.role LIKE '%owner%' OR m.role LIKE '%admin%')`)),
    ];
    for (const { email } of recipients) {
      try {
        await sendEmail({
          to: email,
          template: EmailTemplate.LOW_BALANCE,
          variables: {
            url: new URL(`/dashboard/${org.slug}/credits`, origin).toString(),
            organization: org.name,
            available: sar(available),
            threshold: sar(threshold),
          },
        });
        emailed++;
      } catch (error) {
        logger.error(
          error,
          `low-balance email to a member of ${org.organization_id} failed`,
        );
      }
    }
  }
  return { checked: candidates.length, emailed, cleared };
};
