import { FormEvent, useCallback, useEffect, useState } from "react";
import { ArrowLeft, ChevronDown, ChevronRight, LoaderCircle, Plus, RefreshCw, Search, Trash2 } from "lucide-react";
import type { ApplicationClient, ModelDefinition, ModelProviderConfig, ModelProviderInfo, ProviderModelInfo } from "../contracts";
import { useBackNavigation } from "../primitives/useBackNavigation";
import { InlineConfirmation } from "../primitives/InlineConfirmation";

const errorText = (error: unknown) => error instanceof Error ? error.message : String(error);
const numberText = (value: number) => value >= 1000 ? `${Math.round(value / 1000)}k` : String(value);

function ProviderDetails({ client, cwd, provider, busy, run, onEdit }: {
  client: ApplicationClient; cwd: string; provider: ModelProviderInfo; busy: boolean;
  run(operation: () => Promise<void>): Promise<void>; onEdit(): void;
}) {
  const [models, setModels] = useState<ProviderModelInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [key, setKey] = useState("");
  const [removingCredential, setRemovingCredential] = useState(false);
  useEffect(() => {
    let current = true;
    setLoading(true); setError("");
    void client.providerModels(cwd, provider.id).then((value) => { if (current) setModels(value); })
      .catch((error) => { if (current) setError(errorText(error)); })
      .finally(() => { if (current) setLoading(false); });
    return () => { current = false; };
  }, [client, cwd, provider.id]);
  const query = search.trim().toLocaleLowerCase();
  const visible = models.filter((model) => `${model.name} ${model.id}`.toLocaleLowerCase().includes(query));
  const submitKey = (event: FormEvent) => {
    event.preventDefault();
    void run(async () => { await client.setProviderAPIKey(provider.id, key.trim()); setKey(""); });
  };
  return <div className="pi-provider-details">
    <div className="pi-model-actions">
      {provider.builtin
        ? <button type="button" disabled={busy} onClick={() => void run(() => client.setBuiltinProviderEnabled(provider.id, true, true))}>仅启用此内置渠道</button>
        : <button type="button" disabled={busy} onClick={onEdit}>编辑渠道</button>}
      {provider.credentialType && <button type="button" disabled={busy} onClick={() => setRemovingCredential(true)}>移除{provider.credentialType === "oauth" ? "登录" : "密钥"}</button>}
    </div>
    {removingCredential && <InlineConfirmation message={`移除 ${provider.name} 的保存凭据？`} working={busy}
      onCancel={() => setRemovingCredential(false)} onConfirm={() => void run(async () => {
        await client.deleteProviderCredential(provider.id, provider.credentialType!); setRemovingCredential(false);
      })} />}
    {provider.supportsApiKey && <form className="pi-model-key-form" onSubmit={submitKey}>
      <input type="password" aria-label={`${provider.name} API Key`} placeholder={provider.configured ? "替换 API Key" : "API Key"}
        value={key} autoComplete="off" onChange={(event) => setKey(event.target.value)} />
      <button type="submit" disabled={busy || !key.trim()}>保存密钥</button>
    </form>}
    <div className="pi-model-search"><Search size={15} /><input aria-label={`搜索 ${provider.name} 模型`} placeholder="搜索模型" value={search} onChange={(event) => setSearch(event.target.value)} /></div>
    {error && <p className="pi-settings-error" role="alert">{error}</p>}
    {loading ? <LoaderCircle className="pi-loading-spinner" size={16} aria-label="加载模型" /> : <div className="pi-provider-models">
      {visible.map((model) => <div className="pi-provider-model" key={model.id}>
        <span><strong>{model.name}</strong><small>{model.id}</small></span>
        <small>{model.imageInput ? "图像 · " : ""}{model.reasoning ? "推理 · " : ""}{numberText(model.contextWindow)}</small>
      </div>)}
      {!visible.length && <p className="pi-model-empty">{query ? "没有匹配的模型" : "暂无模型"}</p>}
    </div>}
  </div>;
}

function ProviderEditor({ client, initialId, initial, existingIds, onSave, onCancel }: {
  client: ApplicationClient; initialId: string; initial: ModelProviderConfig; existingIds: string[];
  onSave(id: string, config: ModelProviderConfig): Promise<void>; onCancel(): void;
}) {
  const [id, setId] = useState(initialId);
  const [config, setConfig] = useState<ModelProviderConfig>(() => ({ ...initial, models: initial.models?.map((model) => ({ ...model })) ?? [] }));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [discovered, setDiscovered] = useState<{ id: string; name?: string }[]>([]);
  const [discovering, setDiscovering] = useState(false);
  useBackNavigation(true, onCancel, 40);
  const models = config.models ?? [];
  const updateModel = (index: number, patch: Partial<ModelDefinition>) => setConfig((current) => ({ ...current,
    models: current.models?.map((model, i) => i === index ? { ...model, ...patch } : model) }));
  const field = (name: string, value: unknown) => setConfig((current) => ({ ...current, [name]: value }));
  const addModel = (id = "", name = "") => setConfig((current) => ({ ...current, models: [...(current.models ?? []), { id, ...(name ? { name } : {}) }] }));
  const submit = async (event: FormEvent) => {
    event.preventDefault(); setError("");
    const providerId = id.trim();
    if (!providerId || /[/\\\s]/.test(providerId)) { setError("渠道 ID 不能包含空格或斜线"); return; }
    if (!initialId && existingIds.includes(providerId)) { setError("渠道 ID 已存在"); return; }
    const ids = models.map((model) => model.id.trim());
    if (ids.some((id) => !id) || new Set(ids).size !== ids.length) { setError("模型 ID 不能为空或重复"); return; }
    setBusy(true);
    try {
      await onSave(providerId, { ...config, name: config.name?.trim() || undefined, api: config.api || (initialId ? undefined : "openai-completions"),
        apiKey: config.apiKey?.trim() || undefined, baseUrl: config.baseUrl?.trim() || undefined,
        models: models.map((model) => ({ ...model, id: model.id.trim(), name: model.name?.trim() || undefined })) });
    } catch (error) { setError(errorText(error)); } finally { setBusy(false); }
  };
  const discover = async () => {
    setDiscovering(true); setError("");
    try { setDiscovered((await client.discoverModels(id.trim(), config)).models); }
    catch (error) { setError(errorText(error)); } finally { setDiscovering(false); }
  };
  return <form className="pi-model-form" onSubmit={(event) => void submit(event)}>
    <div className="pi-model-form-grid">
      <label>渠道 ID<input required value={id} disabled={Boolean(initialId)} onChange={(event) => setId(event.target.value)} placeholder="my-provider" /></label>
      <label>名称<input value={config.name ?? ""} onChange={(event) => field("name", event.target.value)} placeholder="可选" /></label>
      <label className="pi-model-form-wide">API 地址<input required={!models.some((model) => typeof model.baseUrl === "string")} type="url" value={config.baseUrl ?? ""} onChange={(event) => field("baseUrl", event.target.value)} placeholder="https://api.example.com/v1" /></label>
      <label>协议<select value={config.api ?? "openai-completions"} onChange={(event) => field("api", event.target.value)}>
        <option value="openai-completions">OpenAI Chat Completions</option><option value="openai-responses">OpenAI Responses</option><option value="anthropic-messages">Anthropic Messages</option>
        {config.api && !["openai-completions", "openai-responses", "anthropic-messages"].includes(config.api) && <option value={config.api}>{config.api}</option>}
      </select></label>
      <label>API Key<input type="password" autoComplete="off" value={config.apiKey ?? ""} onChange={(event) => field("apiKey", event.target.value)} placeholder="可选" /></label>
    </div>
    <div className="pi-model-section-title"><h2>模型</h2><div className="pi-model-actions">
      <button type="button" disabled={busy || discovering || !id.trim() || !config.baseUrl?.trim()} onClick={() => void discover()}>{discovering ? "正在获取…" : "从服务获取"}</button>
      <button type="button" disabled={busy} onClick={() => addModel()}><Plus size={14} />添加模型</button>
    </div></div>
    {discovered.length > 0 && <select aria-label="添加服务模型" value="" onChange={(event) => {
      const model = discovered.find((model) => model.id === event.target.value); if (model) addModel(model.id, model.name);
    }}><option value="">选择服务返回的模型…</option>{discovered.map((model) => <option key={model.id} value={model.id} disabled={models.some((value) => value.id === model.id)}>{model.name || model.id}</option>)}</select>}
    <div className="pi-model-editor-list">{models.map((model, index) => <div className="pi-model-editor-item" key={index}>
      <div className="pi-model-editor-heading">
        <label>模型 ID<input required value={model.id} onChange={(event) => updateModel(index, { id: event.target.value })} /></label>
        <label>名称<input value={model.name ?? ""} onChange={(event) => updateModel(index, { name: event.target.value })} /></label>
        <button className="pi-icon-button" type="button" aria-label={`移除模型 ${model.id || index + 1}`} onClick={() => field("models", models.filter((_, i) => i !== index))}><Trash2 size={15} /></button>
      </div>
      <details><summary>模型参数</summary><div className="pi-model-form-grid">
        <label>上下文长度<input type="number" min="1" placeholder="默认" value={model.contextWindow ?? ""} onChange={(event) => updateModel(index, { contextWindow: event.target.value ? Number(event.target.value) : undefined })} /></label>
        <label>最大输出<input type="number" min="1" placeholder="默认" value={model.maxTokens ?? ""} onChange={(event) => updateModel(index, { maxTokens: event.target.value ? Number(event.target.value) : undefined })} /></label>
        <label className="pi-model-check"><input type="checkbox" checked={Boolean(model.reasoning)} onChange={(event) => updateModel(index, { reasoning: event.target.checked })} />支持推理</label>
        <label className="pi-model-check"><input type="checkbox" checked={model.input?.includes("image") ?? false} onChange={(event) => updateModel(index, { input: event.target.checked ? ["text", "image"] : ["text"] })} />支持图片</label>
      </div></details>
    </div>)}</div>
    {error && <p className="pi-settings-error" role="alert">{error}</p>}
    <div className="pi-model-actions pi-model-save-actions"><button type="button" disabled={busy} onClick={onCancel}>取消</button><button className="is-primary" type="submit" disabled={busy || discovering}>{busy ? "正在保存…" : "保存渠道"}</button></div>
  </form>;
}

export function ModelManagement({ client, cwd }: { client: ApplicationClient; cwd: string }) {
  const [providers, setProviders] = useState<ModelProviderInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [expanded, setExpanded] = useState("");
  const [removing, setRemoving] = useState("");
  const [editor, setEditor] = useState<{ id: string; config: ModelProviderConfig } | null>(null);
  const refresh = useCallback(async () => { setProviders(await client.modelProviders(cwd)); }, [client, cwd]);
  useEffect(() => {
    let current = true;
    setLoading(true);
    void client.modelProviders(cwd).then((value) => { if (current) setProviders(value); })
      .catch((error) => { if (current) setError(errorText(error)); }).finally(() => { if (current) setLoading(false); });
    return () => { current = false; };
  }, [client, cwd]);
  const run = async (operation: () => Promise<void>) => {
    setBusy(true); setError("");
    try { await operation(); await refresh(); } catch (error) { setError(errorText(error)); } finally { setBusy(false); }
  };
  const edit = async (id: string) => {
    await run(async () => { const document = await client.readModelsConfig(); setEditor({ id, config: document.providers[id] ?? {} }); });
  };
  const query = search.trim().toLocaleLowerCase();
  const visible = providers.filter((provider) => `${provider.id} ${provider.name}`.toLocaleLowerCase().includes(query));
  return <>
    <h1>模型</h1>
    <section className="pi-settings-content-section pi-model-management">
      {editor ? <>
        <div className="pi-model-section-title"><button className="pi-model-editor-back" type="button" onClick={() => setEditor(null)}><ArrowLeft size={16} />{editor.id ? "编辑渠道" : "添加渠道"}</button></div>
        <ProviderEditor key={editor.id} client={client} initialId={editor.id} initial={editor.config} existingIds={providers.map((provider) => provider.id)}
          onCancel={() => setEditor(null)} onSave={async (id, config) => { await client.writeModelProvider(id, config); setEditor(null); await refresh(); }} />
      </> : <>
        <div className="pi-model-toolbar"><div className="pi-model-search"><Search size={15} /><input aria-label="搜索渠道" placeholder="搜索渠道" value={search} onChange={(event) => setSearch(event.target.value)} /></div>
          <button className="pi-icon-button" type="button" aria-label="刷新模型配置" disabled={busy} onClick={() => void run(async () => {})}><RefreshCw size={16} /></button>
          <button className="pi-model-add" type="button" onClick={() => setEditor({ id: "", config: { api: "openai-completions", models: [] } })}><Plus size={15} />添加渠道</button></div>
        {error && <p className="pi-settings-error" role="alert">{error}</p>}
        {loading ? <LoaderCircle className="pi-loading-spinner" size={18} aria-label="加载渠道" /> : [true, false].map((builtin) => {
          const group = visible.filter((provider) => provider.builtin === builtin);
          if (!group.length) return null;
          return <div className="pi-model-group" key={String(builtin)}><h2>{builtin ? "内置渠道" : "自定义渠道"}</h2><div className="pi-settings-card">
            {group.map((provider) => <div className="pi-model-provider" key={provider.id}>
              <div className="pi-model-provider-row"><button className="pi-model-provider-title" type="button" aria-expanded={expanded === provider.id} onClick={() => { setExpanded((current) => current === provider.id ? "" : provider.id); setRemoving(""); }}>
                {expanded === provider.id ? <ChevronDown size={15} /> : <ChevronRight size={15} />}<span><strong>{provider.name || provider.id}</strong><small>{provider.credentialType === "oauth" && provider.configured ? "已登录" : provider.configured ? "已配置" : "未配置"} · {provider.modelCount} 个模型</small></span>
              </button>
              {builtin ? <button className="pi-model-switch" type="button" role="switch" aria-checked={provider.enabled} aria-label={`启用 ${provider.name}`} disabled={busy} onClick={() => void run(() => client.setBuiltinProviderEnabled(provider.id, !provider.enabled))}><span /></button>
                : <button className="pi-icon-button" type="button" aria-label={`移除渠道 ${provider.name}`} disabled={busy} onClick={() => setRemoving(provider.id)}><Trash2 size={15} /></button>}</div>
              {removing === provider.id && <InlineConfirmation className="pi-model-remove-confirm" message={`移除 ${provider.name}？`} working={busy}
                onCancel={() => setRemoving("")} onConfirm={() => void run(async () => { await client.writeModelProvider(provider.id, null); setRemoving(""); })} />}
              {expanded === provider.id && <ProviderDetails client={client} cwd={cwd} provider={provider} busy={busy} run={run} onEdit={() => void edit(provider.id)} />}
            </div>)}
          </div></div>;
        })}
        {!loading && !visible.length && <p className="pi-model-empty">没有匹配的渠道</p>}
      </>}
    </section>
  </>;
}
