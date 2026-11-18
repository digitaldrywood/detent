export const MAX_ICON_BYTES = 256 * 1024;

function dimensions(bytes: Uint8Array): readonly [number, number] {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const text = (start: number, length: number) => String.fromCharCode(...bytes.subarray(start, start + length));
  if (bytes.length >= 24 && text(0, 8) === "\x89PNG\r\n\x1a\n" && text(12, 4) === "IHDR") {
    return [view.getUint32(16), view.getUint32(20)];
  }
  if (bytes.length >= 3 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) {
    for (let offset = 2; offset + 4 <= bytes.length;) {
      if (bytes[offset++] !== 0xff) break;
      while (bytes[offset] === 0xff) offset++;
      const marker = bytes[offset++]!;
      if (marker === 0xda || marker === 0xd9) break;
      if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) continue;
      if (offset + 2 > bytes.length) break;
      const size = view.getUint16(offset);
      if (size < 2 || offset + size > bytes.length) break;
      if ([0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf].includes(marker) && size >= 8) {
        return [view.getUint16(offset + 5), view.getUint16(offset + 3)];
      }
      offset += size;
    }
    throw new Error("the JPEG header is invalid.");
  }
  if (bytes.length >= 12 && text(0, 4) === "RIFF" && text(8, 4) === "WEBP") {
    let result: readonly [number, number] | null = null;
    const uint24 = (offset: number) => bytes[offset]! | bytes[offset + 1]! << 8 | bytes[offset + 2]! << 16;
    for (let offset = 12; offset + 8 <= bytes.length;) {
      const kind = text(offset, 4);
      const size = view.getUint32(offset + 4, true);
      const data = offset + 8;
      if (size > bytes.length - data) throw new Error("the WebP header is invalid.");
      if (kind === "ANIM" || kind === "ANMF" || (kind === "VP8X" && size > 0 && (bytes[data]! & 2) !== 0)) throw new Error("animated WebP is not supported.");
      if (kind === "VP8X" && size >= 10) result = [uint24(data + 4) + 1, uint24(data + 7) + 1];
      if (kind === "VP8 " && size >= 10 && text(data + 3, 3) === "\x9d\x01\x2a") result ??= [view.getUint16(data + 6, true) & 0x3fff, view.getUint16(data + 8, true) & 0x3fff];
      if (kind === "VP8L" && size >= 5 && bytes[data] === 0x2f) {
        const bits = view.getUint32(data + 1, true);
        result ??= [(bits & 0x3fff) + 1, ((bits >>> 14) & 0x3fff) + 1];
      }
      offset = data + size + (size & 1);
    }
    if (result) return result;
    throw new Error("the WebP header is invalid.");
  }
  throw new Error("use PNG, JPEG or WebP. SVG and GIF are not supported.");
}

export function validateIconBytes(bytes: Uint8Array): readonly [number, number] {
  if (bytes.length > MAX_ICON_BYTES) throw new Error(`is ${(bytes.length / 1024).toFixed(1)} KB; the limit is 256 KB.`);
  const [width, height] = dimensions(bytes);
  if (width < 64 || height < 64 || width > 1024 || height > 1024) throw new Error(`is ${width} × ${height}; each side must be 64 to 1024 px.`);
  if (Math.max(width, height) * 10 > Math.min(width, height) * 11) throw new Error("must be square within 10%.");
  return [width, height];
}

export interface IconUpload {
  readonly name: string;
  readonly width: number;
  readonly height: number;
  readonly size: number;
  readonly content: string;
  readonly preview: string;
}

export async function prepareIconUpload(file: File): Promise<IconUpload> {
  if (file.size > MAX_ICON_BYTES) throw new Error(`${file.name} is ${(file.size / 1024).toFixed(1)} KB; the limit is 256 KB.`);
  try {
    const bytes = new Uint8Array(await file.arrayBuffer());
    const [width, height] = validateIconBytes(bytes);
    const bitmap = await createImageBitmap(new Blob([bytes]));
    bitmap.close();
    const content = btoa(Array.from(bytes, (byte) => String.fromCharCode(byte)).join(""));
    return { name: file.name, width, height, size: file.size, content, preview: `data:image/${bytes[0] === 0x89 ? "png" : bytes[0] === 0xff ? "jpeg" : "webp"};base64,${content}` };
  } catch (cause) {
    throw new Error(`${file.name} ${cause instanceof Error ? cause.message : "could not be read."}`);
  }
}
