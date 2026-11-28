import { getMetadata } from "~/lib/metadata";
import { AuditLog } from "~/modules/admin/audit/audit-log";

export const generateMetadata = getMetadata({
  title: "admin:audit.header.title",
  description: "admin:audit.header.description",
});

export default function AuditPage() {
  return <AuditLog />;
}
