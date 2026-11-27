import { getMetadata } from "~/lib/metadata";
import { KeySettings } from "~/modules/admin/key-settings/key-settings";

export const generateMetadata = getMetadata({
  title: "admin:keySettings.header.title",
  description: "admin:keySettings.header.description",
});

export default function KeySettingsPage() {
  return <KeySettings />;
}
