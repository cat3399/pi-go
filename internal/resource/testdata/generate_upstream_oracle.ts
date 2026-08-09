import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

type Corpus = {
  upstreamCommit: string;
  nodeVersion: string;
  frontmatter: Record<string, string>;
  prompts: Array<{ location: string; description: string; body: string }>;
};

const here = dirname(fileURLToPath(import.meta.url));
const corpus = JSON.parse(readFileSync(join(here, "upstream_oracle_corpus.json"), "utf8")) as Corpus;
const upstreamRoot = resolve(process.env.PI_UPSTREAM_ROOT ?? join(here, "../../../../pi"));

async function main(): Promise<void> {
  const commit = execFileSync("git", ["-C", upstreamRoot, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  if (commit !== corpus.upstreamCommit) throw new Error(`upstream commit ${commit}, want ${corpus.upstreamCommit}`);
  if (process.version !== corpus.nodeVersion) throw new Error(`Node ${process.version}, want ${corpus.nodeVersion}`);
  const frontmatterModule = await import(pathToFileURL(join(upstreamRoot, "packages/coding-agent/src/utils/frontmatter.ts")).href);
  const promptModule = await import(pathToFileURL(join(upstreamRoot, "packages/coding-agent/src/core/prompt-templates.ts")).href);

  const root = mkdtempSync(join(tmpdir(), "pi-go-resource-oracle-"));
  const agentDir = join(root, "agent");
  const cwd = join(root, "project");
  const additional = join(root, "additional");
  for (const prompt of corpus.prompts) {
    const path = join(root, prompt.location);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, `---\ndescription: ${prompt.description}\n---\n${prompt.body}`);
  }
  const templates = promptModule.loadPromptTemplates({ cwd, agentDir, promptPaths: [join(additional, "review.md")], includeDefaults: true });

  process.stdout.write(`${JSON.stringify({
    upstreamCommit: corpus.upstreamCommit,
    generatedBy: "pinned packages/coding-agent parseFrontmatter/loadPromptTemplates",
    generator: { nodeVersion: process.version, corpus: "upstream_oracle_corpus.json" },
    frontmatter: Object.fromEntries(Object.entries(corpus.frontmatter).map(([name, raw]) => [name, frontmatterModule.parseFrontmatter(raw)])),
    promptSourcesInLoaderOrder: templates.map((template: any) => ({
      description: template.description,
      source: template.sourceInfo.source,
      scope: template.sourceInfo.scope,
      origin: template.sourceInfo.origin,
      baseDir: template.sourceInfo.baseDir?.replace(root, "<root>"),
    })),
    hardeningPolicy: {
      yamlMerge: "reject instead of upstream expansion",
      typedKnownFrontmatter: "reject resource instead of upstream coercive consumption",
      additionalCollision: "first resolved/default resource wins; additional path is appended",
    },
  }, null, 2)}\n`);
}

void main().catch((error) => {
  process.stderr.write(`${error instanceof Error ? error.stack ?? error.message : String(error)}\n`);
  process.exitCode = 1;
});
