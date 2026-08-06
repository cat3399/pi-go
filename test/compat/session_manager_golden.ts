import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import {
	SessionManager,
	type SessionEntry,
	type SessionInfo,
	type SessionTreeNode,
} from "../../../pi/packages/coding-agent/src/core/session-manager.ts";
import type { Message } from "../../../pi/packages/ai/src/types.ts";

type JsonObject = Record<string, unknown>;

const userMessage = (text: string, timestamp: number): Message => ({ role: "user", content: text, timestamp });

const assistantMessage = (text: string, timestamp: number): Message => ({
	role: "assistant",
	content: [{ type: "text", text }],
	api: "openai-completions",
	provider: "compat-provider",
	model: "compat-model",
	usage: {
		input: 1,
		output: 1,
		cacheRead: 0,
		cacheWrite: 0,
		totalTokens: 2,
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
	},
	stopReason: "stop",
	timestamp,
});

class CanonicalIds {
	private readonly names = new Map<string, string>();
	private next = 1;

	name(id: string | null | undefined, preferred?: string): string | null {
		if (id === null || id === undefined) return null;
		const current = this.names.get(id);
		if (current) return current;
		const name = preferred ?? `auto-${this.next++}`;
		this.names.set(id, name);
		return name;
	}
}

const own = (value: JsonObject, key: string): JsonObject => {
	if (!Object.hasOwn(value, key)) return { present: false };
	return { present: true, value: value[key] };
};

const canonicalTimestamp = (value: unknown): JsonObject => ({
	present: typeof value === "string",
	isoMilliseconds:
		typeof value === "string" && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(value),
});

const textContent = (message: JsonObject): string => {
	const content = message.content;
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	return content
		.filter((block): block is JsonObject => typeof block === "object" && block !== null)
		.filter((block) => block.type === "text" && typeof block.text === "string")
		.map((block) => block.text as string)
		.join(" ");
};

const canonicalEntry = (entry: JsonObject, ids: CanonicalIds): JsonObject => {
	const type = String(entry.type);
	const result: JsonObject = {
		type,
		id: ids.name(String(entry.id)),
		parentId: ids.name(typeof entry.parentId === "string" ? entry.parentId : null),
		timestamp: canonicalTimestamp(entry.timestamp),
	};
	switch (type) {
		case "message": {
			const message = entry.message as JsonObject;
			result.message = {
				role: message.role,
				text: textContent(message),
				timestamp: message.timestamp,
				...(message.role === "assistant" ? { provider: message.provider, model: message.model } : {}),
			};
			break;
		}
		case "thinking_level_change":
			result.thinkingLevel = entry.thinkingLevel;
			break;
		case "model_change":
			result.provider = entry.provider;
			result.modelId = entry.modelId;
			break;
		case "custom":
			result.customType = entry.customType;
			break;
		case "session_info":
			result.name = entry.name;
			break;
		case "label":
			result.targetId = ids.name(String(entry.targetId));
			result.label = own(entry, "label");
			break;
		case "compaction":
			result.summary = entry.summary;
			result.firstKeptEntryId = ids.name(String(entry.firstKeptEntryId));
			result.tokensBefore = entry.tokensBefore;
			break;
		case "branch_summary":
			result.fromId = entry.fromId === "root" ? "root" : ids.name(String(entry.fromId));
			result.summary = entry.summary;
			break;
	}
	return result;
};

const canonicalEntries = (entries: readonly JsonObject[], ids: CanonicalIds): JsonObject[] => {
	for (const entry of entries) ids.name(String(entry.id));
	return entries.map((entry) => canonicalEntry(entry, ids));
};

const canonicalAgentMessage = (message: JsonObject, ids: CanonicalIds): JsonObject => {
	const role = String(message.role);
	if (role === "branchSummary") {
		return { role, summary: message.summary, fromId: ids.name(String(message.fromId)) };
	}
	if (role === "compactionSummary") {
		return { role, summary: message.summary, tokensBefore: message.tokensBefore };
	}
	return { role, text: textContent(message) };
};

const canonicalContext = (manager: SessionManager, ids: CanonicalIds): JsonObject => {
	const context = manager.buildSessionContext();
	return {
		entryPath: manager.buildContextEntries().map((entry) => ids.name(entry.id)),
		entryTypes: manager.buildContextEntries().map((entry) => entry.type),
		messages: context.messages.map((message) => canonicalAgentMessage(message as unknown as JsonObject, ids)),
		thinkingLevel: context.thinkingLevel,
		model: context.model,
	};
};

const canonicalTree = (tree: readonly SessionTreeNode[], ids: CanonicalIds): JsonObject[] => {
	const convert = (node: SessionTreeNode): JsonObject => ({
		id: ids.name(node.entry.id),
		type: node.entry.type,
		label: node.label ?? null,
		hasLabelTimestamp: node.labelTimestamp !== undefined,
		children: node.children.map(convert),
	});
	return tree.map(convert);
};

const loadJsonl = (path: string): JsonObject[] =>
	readFileSync(path, "utf8")
		.split("\n")
		.filter((line) => line.trim() !== "")
		.map((line) => JSON.parse(line) as JsonObject);

const canonicalOptionalFields = (entries: readonly JsonObject[]): JsonObject[] => {
	const cases: JsonObject[] = [];
	for (const entry of entries) {
		switch (entry.type) {
			case "session":
				cases.push(
					{ case: "header.timestamp", field: canonicalTimestamp(entry.timestamp) },
					{ case: "header.parentSession", field: own(entry, "parentSession") },
				);
				break;
			case "custom":
				cases.push({ case: `custom.${String(entry.customType)}.data`, field: own(entry, "data") });
				break;
			case "label":
				cases.push({
					case: `label.${Object.hasOwn(entry, "label") ? String(entry.label) : "undefined"}`,
					field: own(entry, "label"),
				});
				break;
			case "compaction":
				cases.push(
					{ case: "compaction.details", field: own(entry, "details") },
					{ case: "compaction.fromHook", field: own(entry, "fromHook") },
					{ case: "compaction.usage", field: own(entry, "usage") },
				);
				break;
			case "branch_summary":
				cases.push(
					{ case: "branch_summary.details", field: own(entry, "details") },
					{ case: "branch_summary.fromHook", field: own(entry, "fromHook") },
					{ case: "branch_summary.usage", field: own(entry, "usage") },
				);
				break;
		}
	}
	return cases;
};

const canonicalPath = (path: string | undefined, paths: ReadonlyMap<string, string>): string | null => {
	if (path === undefined) return null;
	return paths.get(resolve(path)) ?? "$UNKNOWN_PATH";
};

const canonicalSessionInfo = (
	info: SessionInfo,
	paths: ReadonlyMap<string, string>,
	cwds: ReadonlyMap<string, string>,
	sessionIds: ReadonlyMap<string, string>,
): JsonObject => ({
	path: canonicalPath(info.path, paths),
	id: sessionIds.get(info.id) ?? info.id,
	cwd: cwds.get(resolve(info.cwd)) ?? "$UNKNOWN_CWD",
	name: info.name ?? null,
	parentSessionPath: canonicalPath(info.parentSessionPath, paths),
	createdValid: !Number.isNaN(info.created.getTime()),
	modifiedMs: info.modified.getTime(),
	messageCount: info.messageCount,
	firstMessage: info.firstMessage,
	allMessagesText: info.allMessagesText,
});

const optionalAndPersistenceScenario = async (root: string): Promise<JsonObject> => {
	const cwd = join(root, "optional-cwd");
	const dir = join(root, "optional-sessions");
	mkdirSync(cwd, { recursive: true });
	const manager = SessionManager.create(cwd, dir, { id: "compat-optional" });
	const file = manager.getSessionFile();
	assert(file);
	const existsAfterCreate = existsSync(file);
	const ids = new CanonicalIds();
	const rootId = manager.appendMessage(userMessage("optional root", 1000));
	ids.name(rootId, "root");
	const existsAfterUser = existsSync(file);
	manager.appendCustomEntry("undefined");
	manager.appendCustomEntry("null", null);
	manager.appendCustomEntry("false", false);
	manager.appendThinkingLevelChange("high");
	manager.appendModelChange("selected-provider", "selected-model");
	manager.appendSessionInfo("  Golden\r\nSession  ");
	manager.appendLabelChange(rootId, undefined);
	manager.appendLabelChange(rootId, "");
	const compactionId = manager.appendCompaction("optional summary", rootId, 77, null, false);
	ids.name(compactionId, "compaction");
	const existsAfterMetadata = existsSync(file);
	const summaryId = manager.branchWithSummary(rootId, "returned branch", undefined, false);
	ids.name(summaryId, "branch-summary");
	const assistantId = manager.appendMessage(assistantMessage("optional final", 2000));
	ids.name(assistantId, "assistant");
	const existsAfterAssistant = existsSync(file);
	const records = loadJsonl(file);
	const rawEntries = records.filter((entry) => entry.type !== "session");
	const progress: Array<[number, number]> = [];
	const listed = await SessionManager.list(cwd, dir, (loaded, total) => progress.push([loaded, total]));
	const paths = new Map([[resolve(file), "$OPTIONAL_FILE"]]);
	const cwds = new Map([[resolve(cwd), "$OPTIONAL_CWD"]]);
	const sessionIds = new Map([[manager.getSessionId(), "compat-optional"]]);
	return {
		delayedPersistence: { existsAfterCreate, existsAfterUser, existsAfterMetadata, existsAfterAssistant },
		optionalFields: canonicalOptionalFields(records),
		entries: canonicalEntries(rawEntries, ids),
		sessionName: manager.getSessionName() ?? null,
		activeContext: canonicalContext(manager, ids),
		listProgressFinal: progress.at(-1) ?? null,
		list: listed.map((info) => canonicalSessionInfo(info, paths, cwds, sessionIds)),
	};
};

const treeAndSelectionScenario = (root: string): JsonObject => {
	const cwd = join(root, "tree-cwd");
	mkdirSync(cwd, { recursive: true });
	const manager = SessionManager.inMemory(cwd, { id: "compat-tree" });
	const ids = new CanonicalIds();
	const rootId = manager.appendMessage(userMessage("tree root", 10_000));
	ids.name(rootId, "root");
	const thinkingId = manager.appendThinkingLevelChange("high");
	ids.name(thinkingId, "thinking");
	const modelId = manager.appendModelChange("selected-provider", "selected-model");
	ids.name(modelId, "model");
	const assistantId = manager.appendMessage(assistantMessage("tree answer", 11_000));
	ids.name(assistantId, "assistant");
	const abandonedId = manager.appendMessage(userMessage("abandoned", 12_000));
	ids.name(abandonedId, "abandoned");
	manager.branch(modelId);
	const branchId = manager.appendMessage(userMessage("selected branch", 13_000));
	ids.name(branchId, "selected-branch");
	const labelId = manager.appendLabelChange(rootId, "checkpoint");
	ids.name(labelId, "label");
	manager.branch(branchId);
	const selected = canonicalContext(manager, ids);
	const selectedBranch = manager.getBranch().map((entry) => ids.name(entry.id));
	manager.resetLeaf();
	const resetId = manager.appendMessage(userMessage("new root", 14_000));
	ids.name(resetId, "reset-root");
	const reset = canonicalContext(manager, ids);
	const summaryId = manager.branchWithSummary(modelId, "abandoned summary", undefined, false);
	ids.name(summaryId, "branch-summary");
	const summary = canonicalContext(manager, ids);
	const entries = manager.getEntries().map((entry) => JSON.parse(JSON.stringify(entry)) as JsonObject);
	return {
		selected,
		selectedBranch,
		reset,
		summary,
		resolvedLabel: manager.getLabel(rootId) ?? null,
		entries: canonicalEntries(entries, ids),
		tree: canonicalTree(manager.getTree(), ids),
	};
};

const branchedAndForkScenario = async (root: string): Promise<JsonObject> => {
	const sourceCwd = join(root, "source-cwd");
	const targetCwd = join(root, "target-cwd");
	const dir = join(root, "forest-sessions");
	mkdirSync(sourceCwd, { recursive: true });
	mkdirSync(targetCwd, { recursive: true });
	const source = SessionManager.create(sourceCwd, dir, { id: "compat-source" });
	const ids = new CanonicalIds();
	const rootId = source.appendMessage(userMessage("forest root", 100));
	ids.name(rootId, "root");
	const assistantId = source.appendMessage(assistantMessage("forest answer", 200));
	ids.name(assistantId, "assistant");
	const mainId = source.appendMessage(userMessage("main branch", 300));
	ids.name(mainId, "main");
	const labelId = source.appendLabelChange(rootId, "forest-checkpoint");
	ids.name(labelId, "source-label");
	source.branch(assistantId);
	const branchId = source.appendMessage(userMessage("selected branch", 400));
	ids.name(branchId, "selected");
	const sourceFile = source.getSessionFile();
	assert(sourceFile);
	const sourceEntries = source.getEntries().map((entry) => JSON.parse(JSON.stringify(entry)) as JsonObject);
	const sourceTree = canonicalTree(source.getTree(), ids);

	const branchedFile = source.createBranchedSession(branchId);
	assert(branchedFile);
	const branchedId = source.getSessionId();
	const branchedEntries = source.getEntries().map((entry) => JSON.parse(JSON.stringify(entry)) as JsonObject);
	const branchedHeader = source.getHeader();
	assert(branchedHeader);
	const branchedTree = canonicalTree(source.getTree(), ids);

	const fork = SessionManager.forkFrom(sourceFile, targetCwd, dir, { id: "compat-fork" });
	const forkFile = fork.getSessionFile();
	assert(forkFile);
	const forkHeader = fork.getHeader();
	assert(forkHeader);
	const forkEntries = fork.getEntries().map((entry) => JSON.parse(JSON.stringify(entry)) as JsonObject);
	const forkTree = canonicalTree(fork.getTree(), ids);

	const paths = new Map([
		[resolve(sourceFile), "$SOURCE_FILE"],
		[resolve(branchedFile), "$BRANCHED_FILE"],
		[resolve(forkFile), "$FORK_FILE"],
	]);
	const cwds = new Map([
		[resolve(sourceCwd), "$SOURCE_CWD"],
		[resolve(targetCwd), "$TARGET_CWD"],
	]);
	const sessionIds = new Map([
		["compat-source", "compat-source"],
		[branchedId, "compat-branched"],
		["compat-fork", "compat-fork"],
	]);
	const canonicalList = (values: SessionInfo[]): JsonObject[] =>
		values
			.map((info) => canonicalSessionInfo(info, paths, cwds, sessionIds))
			.sort((left, right) => String(left.id).localeCompare(String(right.id)));
	const projectList = await SessionManager.list(sourceCwd, dir);
	const allList = await SessionManager.listAll(dir);
	return {
		source: { entries: canonicalEntries(sourceEntries, ids), tree: sourceTree },
		branched: {
			fileExists: existsSync(branchedFile),
			headerParent: canonicalPath(branchedHeader.parentSession, paths),
			entries: canonicalEntries(branchedEntries, ids),
			tree: branchedTree,
			resolvedLabel: source.getLabel(rootId) ?? null,
		},
		fork: {
			fileExists: existsSync(forkFile),
			headerParent: canonicalPath(forkHeader.parentSession, paths),
			entries: canonicalEntries(forkEntries, ids),
			tree: forkTree,
		},
		projectList: canonicalList(projectList),
		allList: canonicalList(allList),
	};
};

const generate = async (): Promise<JsonObject> => {
	const root = mkdtempSync(join(tmpdir(), "pi-go-session-compat-"));
	return {
		formatVersion: 1,
		optionalAndPersistence: await optionalAndPersistenceScenario(root),
		treeAndSelection: treeAndSelectionScenario(root),
		branchedAndFork: await branchedAndForkScenario(root),
	};
};

const main = async (): Promise<void> => {
	const generated = await generate();
	const encoded = `${JSON.stringify(generated, null, 2)}\n`;
	const [mode, target] = process.argv.slice(2);
	if (mode === "--write") {
		if (!target) throw new Error("--write requires a target path");
		mkdirSync(dirname(resolve(target)), { recursive: true });
		writeFileSync(resolve(target), encoded);
		return;
	}
	if (mode === "--check") {
		if (!target) throw new Error("--check requires a golden path");
		const expected = JSON.parse(readFileSync(resolve(target), "utf8")) as JsonObject;
		assert.deepStrictEqual(generated, expected);
		return;
	}
	process.stdout.write(encoded);
};

await main();
