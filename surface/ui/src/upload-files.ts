export const MAX_UPLOAD_FILE_BYTES = 25 * 1024 * 1024;
export const MAX_UPLOAD_TOTAL_BYTES = 100 * 1024 * 1024;

export interface EncodedUploadFile {
  name: string;
  data: string;
}

export function validateUploadFiles(files: File[]): void {
  if (files.length === 0) throw new Error("请选择要上传的文件");
  const names = new Set<string>();
  let total = 0;
  for (const file of files) {
    if (!file.name || file.name === "." || file.name === ".." || /[\\/\0]/.test(file.name)) {
      throw new Error(`文件名无效：${file.name || "（空）"}`);
    }
    if (names.has(file.name)) throw new Error(`不能同时上传重名文件：${file.name}`);
    names.add(file.name);
    if (file.size > MAX_UPLOAD_FILE_BYTES) {
      throw new Error(`单个文件不能超过 25MB：${file.name}`);
    }
    total += file.size;
  }
  if (total > MAX_UPLOAD_TOTAL_BYTES) throw new Error("单次上传的文件总大小不能超过 100MB");
}

function fileBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const result = String(reader.result ?? "");
      const separator = result.indexOf(",");
      if (separator < 0) {
        reject(new Error(`无法读取文件：${file.name}`));
        return;
      }
      resolve(result.slice(separator + 1));
    };
    reader.onerror = () => reject(reader.error ?? new Error(`无法读取文件：${file.name}`));
    reader.readAsDataURL(file);
  });
}

export async function encodeUploadFiles(files: File[]): Promise<EncodedUploadFile[]> {
  validateUploadFiles(files);
  const encoded: EncodedUploadFile[] = [];
  for (const file of files) {
    encoded.push({ name: file.name, data: await fileBase64(file) });
  }
  return encoded;
}
