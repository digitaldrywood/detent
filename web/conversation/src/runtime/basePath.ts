const BASE_META = "detent-base-path";
const SIGN_IN_META = "detent-sign-in-path";

interface MetaSource {
  querySelector(selector: string): { getAttribute(name: string): string | null } | null;
}

let base: string | undefined;
let signIn: string | undefined;

export function normalizeBasePath(value: string | null | undefined): string {
  const trimmed = (value ?? "").trim().replace(/\/+$/, "");
  if (!trimmed.startsWith("/") || trimmed.startsWith("//")) return "";
  return trimmed;
}

function readMeta(name: string, source: MetaSource | undefined): string | null {
  return source?.querySelector(`meta[name="${name}"]`)?.getAttribute("content") ?? null;
}

export function readBasePath(source: MetaSource | undefined = globalThis.document): string {
  return normalizeBasePath(readMeta(BASE_META, source));
}

export function basePath(): string {
  base ??= readBasePath();
  return base;
}

export function hubPath(path: string, prefix: string = basePath()): string {
  if (prefix === "" || !path.startsWith("/") || path.startsWith("//")) return path;
  if (path === prefix || path.startsWith(`${prefix}/`) || path.startsWith(`${prefix}?`)) {
    return path;
  }
  return `${prefix}${path}`;
}

export function withoutBasePath(pathname: string, prefix: string = basePath()): string {
  if (prefix === "") return pathname;
  if (pathname === prefix) return "/";
  if (pathname.startsWith(`${prefix}/`)) return pathname.slice(prefix.length);
  return pathname;
}

export function routerBasePath(prefix: string = basePath()): string {
  return prefix === "" ? "/" : prefix;
}

export function signInPath(): string {
  signIn ??= readMeta(SIGN_IN_META, globalThis.document) ?? "";
  return signIn.startsWith("/") && !signIn.startsWith("//") ? signIn : hubPath("/login");
}

export function applyHubPaths(paths: {
  readonly base_path?: string | undefined;
  readonly sign_in_path?: string | undefined;
}): void {
  if (paths.base_path !== undefined) base = normalizeBasePath(paths.base_path);
  if (paths.sign_in_path !== undefined) signIn = paths.sign_in_path;
}

export function resetHubPaths(): void {
  base = undefined;
  signIn = undefined;
}
