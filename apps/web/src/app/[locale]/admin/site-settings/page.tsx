import { getMetadata } from "~/lib/metadata";
import { SiteSettings } from "~/modules/admin/site-settings/site-settings";

export const generateMetadata = getMetadata({
  title: "admin:siteSettings.header.title",
  description: "admin:siteSettings.header.description",
});

export default function SiteSettingsPage() {
  return <SiteSettings />;
}
