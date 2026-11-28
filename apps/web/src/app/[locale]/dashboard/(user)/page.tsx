import { redirect } from "next/navigation";

import { pathsConfig } from "~/config/paths";
import { getPersonalAccountForSession } from "~/lib/auth/server";

/**
 * The dashboard opens the user's personal account (ADR 0020); the account
 * switcher leads from there to their organizations. A user who has not
 * finished the welcome wizard is sent to it.
 */
export default async function UserPage() {
  const personal = await getPersonalAccountForSession();

  if (!personal) {
    return redirect(pathsConfig.auth.login);
  }
  // a new user is welcomed first (ONB-03)
  return redirect(
    personal.onboardedAt
      ? pathsConfig.dashboard.organization(personal.slug).index
      : pathsConfig.onboarding,
  );
}
