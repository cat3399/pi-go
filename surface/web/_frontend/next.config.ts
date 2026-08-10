import type { NextConfig } from "next";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const { version } = JSON.parse(
  readFileSync(join(import.meta.dirname, "package.json"), "utf8"),
) as { version: string };

const development = process.env.NODE_ENV === "development";
const apiOrigin = (process.env.PI_GO_WEB_API_ORIGIN ?? "http://127.0.0.1:30142").replace(/\/+$/, "");

const nextConfig: NextConfig = {
	...(development
		? {
				async rewrites() {
					return [{ source: "/api/:path*", destination: `${apiOrigin}/api/:path*` }];
				},
			}
		: { output: "export" as const }),
	images: { unoptimized: true },
  env: {
    NEXT_PUBLIC_APP_VERSION: version,
    NEXT_PUBLIC_PI_VERSION: process.env.PI_GO_VERSION ?? "go-dev",
  },
};

export default nextConfig;
