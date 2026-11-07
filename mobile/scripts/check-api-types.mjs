// Fails if src/api/schema.d.ts is not what the generator makes from ../api/openapi.yaml now
// (the same idea as the sqlc "generate leaves no diff" gate). Fix: npm run gen:api.
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const dir = mkdtempSync(join(tmpdir(), "api-types-"));
try {
  const out = join(dir, "schema.d.ts");
  execFileSync("npx", ["openapi-typescript", "../api/openapi.yaml", "-o", out], { stdio: "ignore" });
  const fresh = readFileSync(out, "utf8");
  const committed = readFileSync("src/api/schema.d.ts", "utf8");
  if (fresh !== committed) {
    console.error("src/api/schema.d.ts is out of date with api/openapi.yaml: run `npm run gen:api`");
    process.exit(1);
  }
  console.log("src/api/schema.d.ts matches api/openapi.yaml");
} finally {
  rmSync(dir, { recursive: true, force: true });
}
