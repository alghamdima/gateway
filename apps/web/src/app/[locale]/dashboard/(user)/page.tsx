import { redirect } from "next/navigation";

import { pathsConfig } from "~/config/paths";
import { getPersonalAccountForSession } from "~/lib/auth/server";

/**
 * The dashboard opens the user's personal account (ADR 0020); the account
 * switcher leads from there to their organizations.
 */
export default async function UserPage() {
  const personal = await getPersonalAccountForSession();

  return redirect(
    personal
      ? pathsConfig.dashboard.organization(personal.slug).index
      : pathsConfig.auth.login,
  );
}
