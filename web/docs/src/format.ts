const ENTITIES: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' };

export const esc = (s: unknown): string => String(s).replace(/[&<>"]/g, (c) => ENTITIES[c] ?? c);

/** Escapes text and turns `code` spans into <code>. */
export const md = (s: string): string => esc(s).replace(/`([^`]+)`/g, '<code>$1</code>');

export const pathHtml = (p: string): string => esc(p).replace(/\{(\w+)\}/g, '<i>{$1}</i>');

export const pretty = (v: unknown): string => JSON.stringify(v, null, 2);

/** Escapes JSON text and wraps keys, strings, numbers and literals in token spans. */
export function highlightJSON(text: string): string {
  return esc(text).replace(
    /(&quot;(?:[^&]|&(?!quot;))*?&quot;)(\s*:)?|\b(true|false|null)\b|(-?\b\d+(?:\.\d+)?\b)/g,
    (_m, str: string | undefined, colon: string | undefined, lit: string | undefined, num: string | undefined) => {
      if (str) return colon ? `<span class="tk-key">${str}</span>${colon}` : `<span class="tk-str">${str}</span>`;
      if (lit) return `<span class="tk-lit">${lit}</span>`;
      return `<span class="tk-num">${num}</span>`;
    },
  );
}

/** Escapes code and marks comments and string literals. */
export function highlightCode(text: string): string {
  return esc(text).replace(
    /((?<![:\w])\/\/[^\n]*|^\s*#[^\n]*$)|(&quot;(?:[^&\n]|&(?!quot;))*?&quot;|'[^'\n]*'|`[^`\n]*`)/gm,
    (_m, com: string | undefined, str: string | undefined) => (com ? `<span class="tk-com">${com}</span>` : `<span class="tk-str">${str}</span>`),
  );
}
