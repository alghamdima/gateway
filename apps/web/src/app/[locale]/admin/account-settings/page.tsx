import { getMetadata } from "~/lib/metadata";
import { AccountSettings } from "~/modules/admin/account-settings/account-settings";

export const generateMetadata = getMetadata({
  title: "admin:accountSettings.header.title",
  description: "admin:accountSettings.header.description",
});

export default function AccountSettingsPage() {
  return <AccountSettings />;
}
