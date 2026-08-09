import type { NextConfig } from "next";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const { version } = JSON.parse(
  readFileSync(join(import.meta.dirname, "package.json"), "utf8"),
) as { version: string };

const nextConfig: NextConfig = {
  output: "export",
  images: { unoptimized: true },
  env: {
    NEXT_PUBLIC_APP_VERSION: version,
    NEXT_PUBLIC_PI_VERSION: process.env.PI_GO_VERSION ?? "go-dev",
  },
};

export default nextConfig;
