export function pastedAttachments(data: DataTransfer) {
  const files = data.files.length ? Array.from(data.files) : Array.from(data.items)
    .filter((item) => item.kind === "file")
    .map((item) => item.getAsFile())
    .filter((file): file is File => file !== null);
  if (!files.length) return null;
  return {
    images: files.filter((file) => file.type.startsWith("image/")),
    uploads: files.filter((file) => !file.type.startsWith("image/")),
    // Let the textarea insert accompanying text at the selection, with native undo.
    hasText: Boolean(data.getData("text/plain")),
  };
}
