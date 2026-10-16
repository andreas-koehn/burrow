/** The relay's limit on a model's or a key's system text. */
export const MAX_SYSTEM_TEXT_BYTES = 8192;

/** UTF-8 bytes: the relay counts bytes, not characters. */
export const byteLength = (s: string) => new TextEncoder().encode(s).length;
