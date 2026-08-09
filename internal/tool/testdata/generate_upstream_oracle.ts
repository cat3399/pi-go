import { constants as bufferConstants } from "node:buffer";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir, platform, arch, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

type Corpus = {
  upstreamCommit: string;
  nodeVersion: string;
  tools: Record<string, { version: string; sha256ByPlatform: Record<string, string> }>;
  files: Record<string, string>;
  grep: { pattern: string; braceGlob: string; negativeGlob: string };
  find: { pattern: string };
  ignore: {
    pattern: string;
    grepPattern: string;
    controlFiles: Record<string, string>;
    files: Record<string, string>;
  };
  ancestorIgnore: {
    findPattern: string;
    grepPattern: string;
    repositorySearch: string;
    outsideRepositorySearch: string;
    controlFiles: Record<string, string>;
    files: Record<string, string>;
  };
};

const here = dirname(fileURLToPath(import.meta.url));
const corpus = JSON.parse(readFileSync(join(here, "upstream_oracle_corpus.json"), "utf8")) as Corpus;
const upstreamRoot = resolve(process.env.PI_UPSTREAM_ROOT ?? join(here, "../../../../pi"));
const toolDirectory = resolve(process.env.PI_ORACLE_TOOL_DIR ?? join(homedir(), ".pi", "agent", "bin"));

function firstLine(command: string): string {
  return execFileSync(command, ["--version"], { encoding: "utf8" }).split("\n")[0] ?? "";
}

function sha256(path: string): string {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function verifyPinnedInputs(): void {
  const commit = execFileSync("git", ["-C", upstreamRoot, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  if (commit !== corpus.upstreamCommit) throw new Error(`upstream commit ${commit}, want ${corpus.upstreamCommit}`);
  if (process.version !== corpus.nodeVersion) throw new Error(`Node ${process.version}, want ${corpus.nodeVersion}`);
  const toolPlatform = `${platform()}-${arch()}`;
  for (const name of ["rg", "fd"] as const) {
    const binary = join(toolDirectory, name);
    const expectedDigest = corpus.tools[name].sha256ByPlatform[toolPlatform];
    if (!expectedDigest) {
      throw new Error(`${name} has no pinned sha256 for oracle platform ${toolPlatform}`);
    }
    const version = firstLine(binary);
    if (version !== corpus.tools[name].version) throw new Error(`${name} ${version}, want ${corpus.tools[name].version}`);
    const digest = sha256(binary);
    if (digest !== expectedDigest) throw new Error(`${name} sha256 ${digest} is not pinned for ${toolPlatform}`);
  }
}

function writeFixture(root: string, files: Record<string, string>): void {
  for (const [name, content] of Object.entries(files)) {
    const target = join(root, name);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, content);
  }
}

function outputLines(value: string): string[] {
  return value.split("\n").filter((line) => line.length > 0 && !line.startsWith("[")).sort();
}

async function main(): Promise<void> {
  verifyPinnedInputs();
  // tools-manager resolves managed binaries when this module is first loaded.
  process.env.PI_CODING_AGENT_DIR = dirname(toolDirectory);
  const grepModule = await import(pathToFileURL(join(upstreamRoot, "packages/coding-agent/src/core/tools/grep.ts")).href);
  const findModule = await import(pathToFileURL(join(upstreamRoot, "packages/coding-agent/src/core/tools/find.ts")).href);

  const root = mkdtempSync(join(tmpdir(), "pi-go-tool-oracle-"));
  const cwd = join(root, "project");
  mkdirSync(cwd, { recursive: true });
  writeFixture(cwd, corpus.files);

  const text = (result: any): string => result.content.map((item: any) => item.text ?? "").join("");
  const grep = grepModule.createGrepToolDefinition(cwd);
  const brace = await grep.execute("oracle", { pattern: corpus.grep.pattern, glob: corpus.grep.braceGlob });
  const negative = await grep.execute("oracle", { pattern: corpus.grep.pattern, glob: corpus.grep.negativeGlob });

  // The oracle result must come from production's default operation, which
  // launches the verified managed fd binary. A separate custom-operation
  // probe records only the pattern forwarded by createFindToolDefinition; its
  // synthetic result is deliberately excluded from the corpus output.
  const find = findModule.createFindToolDefinition(cwd);
  const leadingBang = await find.execute("oracle", { pattern: corpus.find.pattern });
  let forwardedPattern = "";
  const forwardingProbe = findModule.createFindToolDefinition(cwd, {
    operations: {
      exists: () => true,
      glob: (pattern: string) => {
        forwardedPattern = pattern;
        return [];
      },
    },
  });
  await forwardingProbe.execute("oracle-forwarding-probe", { pattern: corpus.find.pattern });

  const ignoreRoot = join(root, "ignore-project");
  mkdirSync(ignoreRoot, { recursive: true });
  writeFixture(ignoreRoot, corpus.ignore.controlFiles);
  writeFixture(ignoreRoot, corpus.ignore.files);
  const ignoreFind = findModule.createFindToolDefinition(ignoreRoot);
  const ignoreFindResult = await ignoreFind.execute("oracle-ignore-find", { pattern: corpus.ignore.pattern });
  const ignoreGrep = grepModule.createGrepToolDefinition(ignoreRoot);
  const ignoreGrepResult = await ignoreGrep.execute("oracle-ignore-grep", { pattern: corpus.ignore.grepPattern });

  const ancestorRoot = join(root, "ancestor-ignore-project");
  mkdirSync(ancestorRoot, { recursive: true });
  writeFixture(ancestorRoot, corpus.ancestorIgnore.controlFiles);
  writeFixture(ancestorRoot, corpus.ancestorIgnore.files);
  const repositorySearch = join(ancestorRoot, corpus.ancestorIgnore.repositorySearch);
  const outsideRepositorySearch = join(ancestorRoot, corpus.ancestorIgnore.outsideRepositorySearch);
  const repositoryFindResult = await findModule.createFindToolDefinition(repositorySearch).execute(
    "oracle-ancestor-repository-find",
    { pattern: corpus.ancestorIgnore.findPattern },
  );
  const repositoryGrepResult = await grepModule.createGrepToolDefinition(repositorySearch).execute(
    "oracle-ancestor-repository-grep",
    { pattern: corpus.ancestorIgnore.grepPattern },
  );
  const outsideFindResult = await findModule.createFindToolDefinition(outsideRepositorySearch).execute(
    "oracle-ancestor-outside-find",
    { pattern: corpus.ancestorIgnore.findPattern },
  );
  const outsideGrepResult = await grepModule.createGrepToolDefinition(outsideRepositorySearch).execute(
    "oracle-ancestor-outside-grep",
    { pattern: corpus.ancestorIgnore.grepPattern },
  );

  process.stdout.write(`${JSON.stringify({
    upstreamCommit: corpus.upstreamCommit,
    generatedBy: "pinned packages/coding-agent default rg/fd operations; custom Find probe records forwarding only",
    generator: {
      nodeVersion: process.version,
      toolPlatform: `${platform()}-${arch()}`,
      rgVersion: firstLine(join(toolDirectory, "rg")),
      rgSHA256: sha256(join(toolDirectory, "rg")),
      fdVersion: firstLine(join(toolDirectory, "fd")),
      fdSHA256: sha256(join(toolDirectory, "fd")),
      corpus: "upstream_oracle_corpus.json",
    },
    readRuntimeBoundary: {
      nodeVersion: process.version,
      bufferMaxStringLength: bufferConstants.MAX_STRING_LENGTH,
      unit: "decoded UTF-16 code units",
    },
    grepGlob: {
      brace: text(brace).split("\n").filter(Boolean).sort(),
      negative: text(negative).split("\n").filter(Boolean).sort(),
    },
    findGlob: {
      input: corpus.find.pattern,
      forwardedPattern,
      operation: "default createFindToolDefinition with verified managed fd",
      output: text(leadingBang).trim(),
    },
    ignoreSemantics: {
      operation: "default createFindToolDefinition/createGrepToolDefinition with verified managed fd/rg",
      find: outputLines(text(ignoreFindResult)),
      grep: outputLines(text(ignoreGrepResult)),
    },
    ancestorIgnoreSemantics: {
      operation: "default managed fd/rg ancestor discovery",
      repositoryFind: outputLines(text(repositoryFindResult)),
      repositoryGrep: outputLines(text(repositoryGrepResult)),
      outsideRepositoryFind: outputLines(text(outsideFindResult)),
      outsideRepositoryGrep: outputLines(text(outsideGrepResult)),
    },
  }, null, 2)}\n`);
}

void main().catch((error) => {
  process.stderr.write(`${error instanceof Error ? error.stack ?? error.message : String(error)}\n`);
  process.exitCode = 1;
});
