import { defineConfig } from "tsup";

export default defineConfig({
  entry: ["src/index.ts", "src/node.ts"],
  format: ["cjs", "esm"],
  dts: true,
  clean: true,
  sourcemap: true,
  // One copy of the core for both entries in CJS too, so a RetryError thrown
  // from a handler imported from "entroq" is the class entroq/node checks.
  splitting: true,
});
