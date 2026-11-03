import { AtSign, Download, Eye, Trash2 } from "lucide-react";
import { ContextMenu, useContextMenu, type ContextMenuItem } from "../primitives/ContextMenu";

type FileAction = "preview" | "mention" | "download" | "delete";

export function useFileContextMenu(onAction: (action: FileAction) => void) {
  return useContextMenu<FileAction>(onAction);
}

export function FileContextMenu({ name, isDir, downloading, menu }: {
  name: string;
  isDir: boolean;
  downloading: boolean;
  menu: ReturnType<typeof useFileContextMenu>;
}) {
  const items: ContextMenuItem<FileAction>[] = [];
  if (!isDir) items.push({ id: "preview", label: "预览", icon: <Eye size={15} /> });
  items.push({ id: "mention", label: "引用", icon: <AtSign size={15} /> });
  if (!isDir) items.push({ id: "download", label: downloading ? "正在下载…" : "下载", icon: <Download size={15} />, disabled: downloading });
  items.push({ id: "delete", label: "删除", icon: <Trash2 size={15} />, danger: true });
  return <ContextMenu name={name} items={items} menu={menu} />;
}
