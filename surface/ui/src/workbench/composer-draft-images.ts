export interface DraftImageData {
  data: string;
  mimeType: string;
}

/** Images never leave the device as part of draft synchronization. */
export class DraftImageStore {
  private database: Promise<IDBDatabase> | undefined;
  private readonly fallback = new Map<string, DraftImageData>();
  private readonly namespace: string;

  constructor(namespace: string) {
    this.namespace = namespace;
  }

  private open(): Promise<IDBDatabase> {
    if (!this.database) {
      this.database = new Promise((resolve, reject) => {
        if (!globalThis.indexedDB) { reject(new Error("本机图片存储不可用")); return; }
        const request = indexedDB.open("pi-composer-images", 1);
        request.onupgradeneeded = () => request.result.createObjectStore("images");
        request.onerror = () => reject(request.error);
        request.onsuccess = () => resolve(request.result);
      });
      void this.database.catch(() => { this.database = undefined; });
    }
    return this.database;
  }

  async put(id: string, value: DraftImageData): Promise<boolean> {
    const key = `${this.namespace}\0${id}`;
    try {
      const db = await this.open();
      await new Promise<void>((resolve, reject) => {
        const transaction = db.transaction("images", "readwrite");
        transaction.objectStore("images").put(value, key);
        transaction.oncomplete = () => resolve();
        transaction.onerror = () => reject(transaction.error);
        transaction.onabort = () => reject(transaction.error);
      });
      this.fallback.delete(key);
      return true;
    } catch {
      this.fallback.set(key, value);
      return false;
    }
  }

  async get(id: string): Promise<DraftImageData> {
    const key = `${this.namespace}\0${id}`;
    const fallback = this.fallback.get(key);
    if (fallback) return fallback;
    const db = await this.open();
    return new Promise((resolve, reject) => {
      const request = db.transaction("images", "readonly").objectStore("images").get(key);
      request.onsuccess = () => request.result
        ? resolve(request.result as DraftImageData)
        : reject(new Error("本机图片未能恢复，请重新添加"));
      request.onerror = () => reject(request.error);
    });
  }
}
