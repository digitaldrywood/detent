import { writeFile } from "node:fs/promises";
const { default: dynamicIconImports } = await import(new URL("../node_modules/lucide-react/dist/esm/dynamicIconImports.js", import.meta.url).href);

await writeFile(new URL("../../../internal/projecticon/lucide.txt", import.meta.url), Object.keys(dynamicIconImports).sort().join("\n") + "\n");
