import { Suspense } from "react";

import { getMetadata } from "~/lib/metadata";
import { RequestLog } from "~/modules/admin/requests/request-log";

export const generateMetadata = getMetadata({
  title: "admin:requests.header.title",
  description: "admin:requests.header.description",
});

export default function RequestsPage() {
  return (
    // the log keeps its filters in the URL, which a client component reads
    <Suspense>
      <RequestLog />
    </Suspense>
  );
}
