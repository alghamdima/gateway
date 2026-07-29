import { redirect } from "next/navigation";

import { Flag } from "@workspace/flags";
import { getBooleanValue } from "@workspace/flags-web/server";
import { getTranslation } from "@workspace/i18n/server";
import { cn } from "@workspace/ui";
import { badgeVariants } from "@workspace/ui-web/badge";

import { pathsConfig } from "~/config/paths";
import { getSession } from "~/lib/auth/server";
import { getMetadata } from "~/lib/metadata";
import { DeleteAccount } from "~/modules/user/settings/general/delete-account";
import { EditAvatar } from "~/modules/user/settings/general/edit-avatar";
import { EditName } from "~/modules/user/settings/general/edit-name";
import { LanguageSwitcher } from "~/modules/user/settings/general/language-switcher";

export const generateMetadata = getMetadata({
  title: "auth:account.settings.title",
  description: "auth:account.settings.header.description",
});

export default async function SettingsPage() {
  const { user } = await getSession();

  if (!user) {
    return redirect(pathsConfig.auth.login);
  }

  const { t } = await getTranslation({ ns: "marketing" });
  const demo = await getBooleanValue(Flag.DEMO, false, {
    targetingKey: user.id,
  });

  return (
    <>
      <EditAvatar user={user} />
      <EditName user={user} />
      <LanguageSwitcher />
      <DeleteAccount user={user} />
      {demo ? (
        <a
          className={cn(badgeVariants({ variant: "secondary" }), "self-center")}
          href="https://www.turbostarter.dev/docs/web/flags/overview"
        >
          {t("featureFlags.demo.banner", { flag: Flag.DEMO })}
        </a>
      ) : null}
    </>
  );
}
