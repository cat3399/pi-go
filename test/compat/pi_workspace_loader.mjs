import { pathToFileURL } from "node:url";

// The compatibility runner executes TypeScript source directly without
// installing workspace packages. Resolve the one runtime workspace export used
// by SessionManager to its real source implementation; all other imports use
// Node's normal resolver. Type-only workspace imports are erased by Node.
const piAiUrl = pathToFileURL(
	new URL("../../../pi/packages/ai/src/utils/uuid.ts", import.meta.url).pathname,
).href;
const crossSpawnUrl = new URL("./cross_spawn_shim.mjs", import.meta.url).href;

export async function resolve(specifier, context, nextResolve) {
	if (specifier === "@earendil-works/pi-ai") {
		return { url: piAiUrl, shortCircuit: true };
	}
	if (specifier === "cross-spawn") {
		return { url: crossSpawnUrl, shortCircuit: true };
	}
	return nextResolve(specifier, context);
}
