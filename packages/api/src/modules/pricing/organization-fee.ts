import { desc, eq } from "@workspace/db";
import {
  organization,
  organizationTopupFee,
  pricingChange,
  user,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { resolveTopupFee } from "./fee";
import { getPricingSettings } from "./pricing";

import type { EffectiveTopupFee, TopupFeeRates } from "./fee";

/**
 * A top-up fee for one organization, set by staff (ADMIN-24). Which fee a
 * top-up pays is decided by resolveTopupFee (./fee.ts) and nowhere else; this
 * file reads and writes the organization's row. Every change is written to
 * pricing_change (target "organization:<id>") in the same transaction, and the
 * admin route adds it to the audit log.
 */

export interface OrganizationTopupFee {
  /** 100 bps = 1%; null uses the default. */
  feeBps: number | null;
  /** micro-SAR; null uses the default. */
  feeMinMicroSar: number | null;
  expiresAt: Date | null;
  note: string;
  updatedAt: Date;
  updatedBy: string;
}

/** What the admin card shows: the default, the organization's fee, and what applies now. */
export interface OrganizationTopupFeeView {
  default: TopupFeeRates;
  override: OrganizationTopupFee | null;
  effective: EffectiveTopupFee;
}

const changeTarget = (organizationId: string) =>
  `organization:${organizationId}`;

const columns = {
  feeBps: organizationTopupFee.feeBps,
  feeMinMicroSar: organizationTopupFee.feeMinMicroSar,
  expiresAt: organizationTopupFee.expiresAt,
  note: organizationTopupFee.note,
  updatedAt: organizationTopupFee.updatedAt,
  updatedBy: organizationTopupFee.updatedBy,
};

const findOverride = async (
  organizationId: string,
): Promise<OrganizationTopupFee | null> => {
  const [row] = await db
    .select(columns)
    .from(organizationTopupFee)
    .where(eq(organizationTopupFee.organizationId, organizationId));
  return row ?? null;
};

/** The fee a top-up of this organization pays at `at`, and where it came from. */
export const getEffectiveTopupFee = async (
  organizationId: string,
  at: Date = new Date(),
): Promise<EffectiveTopupFee> => {
  const [settings, override] = await Promise.all([
    getPricingSettings(),
    findOverride(organizationId),
  ]);
  return resolveTopupFee(settings, override, at);
};

export const getOrganizationTopupFee = async (
  organizationId: string,
  at: Date = new Date(),
): Promise<OrganizationTopupFeeView> => {
  const [settings, override] = await Promise.all([
    getPricingSettings(),
    findOverride(organizationId),
  ]);
  return {
    default: {
      topupFeeBps: settings.topupFeeBps,
      topupFeeMinMicroSar: settings.topupFeeMinMicroSar,
    },
    override,
    effective: resolveTopupFee(settings, override, at),
  };
};

/** The organization's fee changes, newest first, with who made each one. */
export const getOrganizationTopupFeeChanges = async (
  organizationId: string,
  limit = 20,
) =>
  db
    .select({
      id: pricingChange.id,
      before: pricingChange.before,
      after: pricingChange.after,
      changedAt: pricingChange.changedAt,
      changedBy: pricingChange.changedBy,
      changedByName: user.name,
      changedByEmail: user.email,
    })
    .from(pricingChange)
    .leftJoin(user, eq(user.id, pricingChange.changedBy))
    .where(eq(pricingChange.target, changeTarget(organizationId)))
    .orderBy(desc(pricingChange.id))
    .limit(limit);

/** The values a change records, as JSON: the audit log and pricing_change keep the same. */
export interface OrganizationTopupFeeValues extends Record<string, unknown> {
  feeBps: number | null;
  feeMinMicroSar: number | null;
  expiresAt: string | null;
  note: string;
}

export interface OrganizationTopupFeeChange {
  override: OrganizationTopupFee | null;
  before: OrganizationTopupFeeValues | null;
  after: OrganizationTopupFeeValues | null;
}

const toValues = (
  fee: Pick<
    OrganizationTopupFee,
    "feeBps" | "feeMinMicroSar" | "expiresAt" | "note"
  > | null,
): OrganizationTopupFeeValues | null =>
  fee && {
    feeBps: fee.feeBps,
    feeMinMicroSar: fee.feeMinMicroSar,
    expiresAt: fee.expiresAt?.toISOString() ?? null,
    note: fee.note,
  };

export interface SetOrganizationTopupFeeInput {
  organizationId: string;
  staffId: string;
  feeBps: number | null;
  feeMinMicroSar: number | null;
  expiresAt: Date | null;
  note: string;
}

/**
 * Sets the organization's fee. The organization's row is locked so two staff
 * saving at once are recorded in order, each with the value it replaced.
 */
export const setOrganizationTopupFee = async ({
  organizationId,
  staffId,
  feeBps,
  feeMinMicroSar,
  expiresAt,
  note,
}: SetOrganizationTopupFeeInput): Promise<OrganizationTopupFeeChange> => {
  if (feeBps === null && feeMinMicroSar === null) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      message: "Set a fee percentage or a minimum, or clear the fee instead",
    });
  }
  if (expiresAt !== null && expiresAt.getTime() <= Date.now()) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      message: "The expiry must be in the future",
    });
  }

  return db.transaction(async (tx) => {
    await lockOrganization(tx, organizationId);
    const [before] = await tx
      .select(columns)
      .from(organizationTopupFee)
      .where(eq(organizationTopupFee.organizationId, organizationId));

    const values = {
      feeBps,
      feeMinMicroSar,
      expiresAt,
      note,
      updatedAt: new Date(),
      updatedBy: staffId,
    };
    const [after] = await tx
      .insert(organizationTopupFee)
      .values({ organizationId, ...values })
      .onConflictDoUpdate({
        target: organizationTopupFee.organizationId,
        set: values,
      })
      .returning(columns);
    if (!after) {
      throw new Error("organization_topup_fee upsert returned no row");
    }

    const change = { before: toValues(before ?? null), after: toValues(after) };
    await tx.insert(pricingChange).values({
      target: changeTarget(organizationId),
      ...change,
      changedBy: staffId,
    });
    return { override: after, ...change };
  });
};

/** Back to the default. Clearing a fee that is not set changes and records nothing. */
export const clearOrganizationTopupFee = async ({
  organizationId,
  staffId,
}: {
  organizationId: string;
  staffId: string;
}): Promise<OrganizationTopupFeeChange> =>
  db.transaction(async (tx) => {
    await lockOrganization(tx, organizationId);
    const [before] = await tx
      .delete(organizationTopupFee)
      .where(eq(organizationTopupFee.organizationId, organizationId))
      .returning(columns);
    if (!before) {
      return { override: null, before: null, after: null };
    }

    const change = { before: toValues(before), after: null };
    await tx.insert(pricingChange).values({
      target: changeTarget(organizationId),
      ...change,
      changedBy: staffId,
    });
    return { override: null, ...change };
  });

type Transaction = Parameters<Parameters<typeof db.transaction>[0]>[0];

const lockOrganization = async (tx: Transaction, organizationId: string) => {
  const [org] = await tx
    .select({ id: organization.id })
    .from(organization)
    .where(eq(organization.id, organizationId))
    .for("update");
  if (!org) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "error.notFound",
    });
  }
};
