// Android 11 devices may still ship a Chromium 87 WebView. The shared
// markdown renderer and paging code also use newer built-in methods. Install
// compatibility shims before importing React or any Workbench dependency.
if (typeof Object.hasOwn !== "function") {
  Object.defineProperty(Object, "hasOwn", {
    configurable: true,
    writable: true,
    value(object: object, property: PropertyKey): boolean {
      return Object.prototype.hasOwnProperty.call(object, property);
    },
  });
}

if (typeof Array.prototype.at !== "function") {
  Object.defineProperty(Array.prototype, "at", {
    configurable: true,
    writable: true,
    value<T>(this: ArrayLike<T>, index: number): T | undefined {
      const length = Math.min(Math.max(Math.trunc(this.length) || 0, 0), Number.MAX_SAFE_INTEGER);
      const relative = Math.trunc(index) || 0;
      const position = relative < 0 ? length + relative : relative;
      return position >= 0 && position < length ? this[position] : undefined;
    },
  });
}
