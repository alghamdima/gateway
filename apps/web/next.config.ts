import type { NextConfig } from "next";

import env from "./env.config";

const INTERNAL_PACKAGES = [
  "@workspace/analytics-web",
  "@workspace/api",
  "@workspace/auth",
  "@workspace/billing",
  "@workspace/cms",
  "@workspace/email",
  "@workspace/db",
  "@workspace/i18n",
  "@workspace/monitoring-web",
  "@workspace/shared",
  "@workspace/storage",
  "@workspace/ui",
  "@workspace/ui-web",
];

const config: NextConfig = {
  reactStrictMode: true,
  turbopack: {
    rules: {
      "*.svg": {
        loaders: ["@svgr/webpack"],
        as: "*.js",
      },
    },
  },

  images: {
    remotePatterns: [
      {
        hostname: "images.unsplash.com",
      },
    ],
  },

  /** Enables hot reloading for local packages without a build step */
  transpilePackages: INTERNAL_PACKAGES,
  experimental: {
    optimizePackageImports: INTERNAL_PACKAGES,
  },
};

const withBundleAnalyzer = require("@next/bundle-analyzer")({
  enabled: env.ANALYZE,
});

export default withBundleAnalyzer(config);
