import { redirect } from "next/navigation";

import { Icons } from "@workspace/ui-web/icons";

import { appConfig } from "~/config/app";
import { pathsConfig } from "~/config/paths";
import { getPersonalAccountForSession } from "~/lib/auth/server";
import { getMetadata } from "~/lib/metadata";
import { getCatalog } from "~/modules/marketing/models/catalog";
import { OnboardingWizard } from "~/modules/onboarding/onboarding-wizard";

export const generateMetadata = getMetadata({
  title: "common:onboarding.welcome.title",
  description: "common:onboarding.welcome.description",
});

/** The welcome wizard (ONB-03): shown once, to a signed-in user who has not finished it. */
export default async function OnboardingPage() {
  const personal = await getPersonalAccountForSession();

  if (!personal) {
    return redirect(pathsConfig.auth.login);
  }
  if (personal.onboardedAt) {
    return redirect(pathsConfig.dashboard.organization(personal.slug).index);
  }

  // the newest model on offer, for the first-request snippet; none when the gateway is away
  const model = (await getCatalog())?.at(0)?.id ?? null;

  return (
    <main className="flex w-full flex-1 flex-col items-center gap-8 p-6 sm:py-12">
      <div className="flex items-center gap-3">
        <Icons.Logo className="text-primary h-8" />
        <Icons.LogoText className="text-foreground h-4" />
      </div>
      <div className="w-full max-w-2xl">
        <OnboardingWizard
          personal={{ id: personal.id, slug: personal.slug }}
          apiUrl={`${appConfig.url}/api/v1`}
          model={model}
        />
      </div>
    </main>
  );
}
