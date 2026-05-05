// Vanilla "build" — Wails just needs a `frontend/dist/` directory it can
// embed. We have no bundler, so just copy index.html and src/ into dist/.

import { cpSync, mkdirSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// fileURLToPath properly decodes URL-encoded characters (spaces, etc.)
// so this works even when the project lives in a path with spaces.
const root = dirname(fileURLToPath(import.meta.url));
const dist = join(root, "dist");

rmSync(dist, { recursive: true, force: true });
mkdirSync(dist, { recursive: true });
cpSync(join(root, "index.html"), join(dist, "index.html"));
cpSync(join(root, "src"), join(dist, "src"), { recursive: true });

console.log("frontend built ->", dist);
