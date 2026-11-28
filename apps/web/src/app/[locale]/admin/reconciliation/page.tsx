import { getMetadata } from "~/lib/metadata";
import { ReconciliationRuns } from "~/modules/admin/reconciliation/reconciliation-runs";

export const generateMetadata = getMetadata({
  title: "admin:reconciliation.title",
  description: "admin:reconciliation.description",
});

export default function ReconciliationPage() {
  return <ReconciliationRuns />;
}
