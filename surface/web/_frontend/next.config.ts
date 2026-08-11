import type { NextConfig } from "next";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const { version } = JSON.parse(
  readFileSync(join(import.meta.dirname, "package.json"), "utf8"),
) as { version: string };

const development = process.env.NODE_ENV === "development";
const apiOrigin = (process.env.PI_GO_WEB_API_ORIGIN ?? "http://127.0.0.1:30142").replace(/\/+$/, "");
// Agent, bash and compaction commands may legitimately outlive Next's 30s
// development rewrite default. Production serves the API directly from Go.
const developmentProxyTimeoutMs = 24 * 60 * 60 * 1_000;

const nextConfig: NextConfig = {
	// Next's development rewrite otherwise gzip-buffers the long-lived SSE
	// response, so browsers see an open connection but no application events.
	compress: false,
	...(development
		? {
				experimental: { proxyTimeout: developmentProxyTimeoutMs },
				async rewrites() {
					return [{ source: "/api/v1/:path*", destination: `${apiOrigin}/api/v1/:path*` }];
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
