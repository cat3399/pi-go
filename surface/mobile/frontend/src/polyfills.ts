// Android 11 devices may still ship a Chromium 87 WebView. The shared
// markdown renderer uses Object.hasOwn (Chromium 93+), so install the tiny
// compatibility shim before importing React or any Workbench dependency.
if (typeof Object.hasOwn !== "function") {
  Object.defineProperty(Object, "hasOwn", {
    configurable: true,
    writable: true,
    value(object: object, property: PropertyKey): boolean {
      return Object.prototype.hasOwnProperty.call(object, property);
    },
  });
}
