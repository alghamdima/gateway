import { getMetadata } from "~/lib/metadata";
import { CatalogSettings } from "~/modules/admin/catalog-settings/catalog-settings";

export const generateMetadata = getMetadata({
  title: "admin:catalogSettings.header.title",
  description: "admin:catalogSettings.header.description",
});

export default function CatalogSettingsPage() {
  return <CatalogSettings />;
}
