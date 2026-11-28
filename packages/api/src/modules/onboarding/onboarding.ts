import { completeOnboarding, getPersonalAccount } from "@workspace/auth/server";
import { and, eq, isNull } from "@workspace/db";
import { ACCOUNT_TYPES, member, personalAccount } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { createKey } from "../keys/keys";

import type { AccountType } from "@workspace/db/schema";
import type { GatewayClient } from "@workspace/gateway-client/server";

/**
 * The welcome wizard a new user sees once (ONB-03, ONB-04, ONB-08; ADR 0020):
 * individual or organization, the organization's set-up if chosen, then a
 * starter API key and a first request. Its state is on the user's personal
 * account. Creating the organization and inviting people are the product's
 * own routes; this module mints the starter key and ends the wizard.
 */

export { ACCOUNT_TYPES };

/** The name the starter key is listed under; the customer can rename it. */
export const STARTER_KEY_NAME = "Starter key";

const refusal = (reason: "completed" | "starterKeyExists" | "notOwner") =>
  new HttpException(
    reason === "notOwner" ? HttpStatusCode.FORBIDDEN : HttpStatusCode.CONFLICT,
    { code: `common:onboarding.error.${reason}` },
  );

export const getOnboarding = async (userId: string) => {
  const personal = await getPersonalAccount(userId);
  const [row] = await db
    .select({ starterKeyId: personalAccount.starterKeyId })
    .from(personalAccount)
    .where(eq(personalAccount.userId, userId));
  return {
    isCompleted: personal.onboardedAt !== null,
    personal: { id: personal.id, slug: personal.slug },
    hasStarterKey: !!row?.starterKeyId,
  };
};

/**
 * Mints the starter key in the account the user chose to start in: their
 * personal account, or an organization they own. Once: the key's value is
 * shown only now, so a second call is refused instead of leaving a key
 * nobody has the value of. It starts from the platform's key defaults
 * (ADMIN-12): the default limit, if staff set one, caps it in case it leaks.
 */
export const createStarterKey = async (
  { userId, organizationId }: { userId: string; organizationId: string },
  gateway?: GatewayClient,
) => {
  const state = await getOnboarding(userId);
  if (state.isCompleted) {
    throw refusal("completed");
  }
  if (state.hasStarterKey) {
    throw refusal("starterKeyExists");
  }
  if (organizationId !== state.personal.id) {
    const [owner] = await db
      .select({ id: member.id })
      .from(member)
      .where(
        and(
          eq(member.organizationId, organizationId),
          eq(member.userId, userId),
          eq(member.role, "owner"),
        ),
      );
    if (!owner) {
      throw refusal("notOwner");
    }
  }

  const { key, apiKey } = await createKey(
    { organizationId, userId, name: STARTER_KEY_NAME },
    gateway,
  );
  await db
    .update(personalAccount)
    .set({ starterKeyId: apiKey.id })
    .where(
      and(
        eq(personalAccount.userId, userId),
        isNull(personalAccount.starterKeyId),
      ),
    );
  return {
    key,
    limitMicroSar: apiKey.limitMicroSar,
    limitReset: apiKey.limitReset,
    expiresAt: apiKey.expiresAt,
  };
};

/** Ends the wizard and keeps what the user said they are. */
export const finishOnboarding = async (
  userId: string,
  accountType: AccountType,
) => {
  await completeOnboarding(userId, accountType);
  return getOnboarding(userId);
};
