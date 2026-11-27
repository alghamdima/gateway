import { expect, test } from "@playwright/test";

import { pathsConfig } from "~/config/paths";

// The admin pages read the admin API, which refuses a signed-out visitor; the
// layout must redirect before the page renders, or the refusal shows as a 500.
test("a signed-out visitor opening an admin page is sent to sign in", async ({
  page,
}) => {
  for (const path of [pathsConfig.admin.index, pathsConfig.admin.users.index]) {
    const response = await page.goto(path);

    expect(response?.status()).toBeLessThan(500);
    await expect(page).toHaveURL(/\/auth\/login/);
  }
});
