import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  resolve: {
    dedupe: ["react", "react-dom"],
  },
  server: {
    host: "127.0.0.1",
    port: 9246,
    strictPort: true,
  },
  build: {
    // Android API 26 is the platform floor. Keep emitted syntax compatible
    // with the Chromium 87 WebView found on the current Android 11 test device.
    target: "chrome87",
  },
});
