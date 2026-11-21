import baseConfig from "@workspace/oxfmt/config";

export default {
  ...baseConfig,
  // the gateway is a Go app with its own npm UI and tooling
  ignorePatterns: [...(baseConfig.ignorePatterns ?? []), "apps/gateway/**"],
};
