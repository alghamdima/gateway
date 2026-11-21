import { defineConfig } from "oxlint";

import baseConfig from "@workspace/oxlint/base";

export default defineConfig({
  extends: [baseConfig],
  // the gateway is a Go app with its own npm UI and tooling
  ignorePatterns: ["apps/gateway/**"],
  options: {
    typeAware: true,
    typeCheck: true,
  },
});
