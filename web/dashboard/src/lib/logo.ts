import { apiOrigin } from './origin';

const LOGO_TYPES = ['image/png', 'image/jpeg', 'image/webp'];
const LOGO_MAX_BYTES = 512 * 1024;
/** The server's size limits (internal/logo), so a 4K image is refused before it is sent. */
const LOGO_MIN_SIDE = 32;
const LOGO_MAX_SIDE = 4096;
const LOGO_MAX_PIXELS = 4 * 1024 * 1024;

/** A friendly reason the file cannot be a checkout logo, or null when it can be sent to the server. */
export function logoFileProblem(file: Pick<File, 'type' | 'size'>): string | null {
  // Some systems report no type for a .webp: the server reads the format from the bytes anyway.
  if (file.type !== '' && !LOGO_TYPES.includes(file.type)) return 'The logo must be a PNG, JPEG or WebP file.';
  if (file.size > LOGO_MAX_BYTES) return 'The logo is larger than 512 KB. Choose a smaller file.';
  return null;
}

/** The image's size as the browser reads it, or null when it cannot (the server then decides). */
async function measure(file: Blob): Promise<{ width: number; height: number } | null> {
  if (typeof createImageBitmap !== 'function') return null;
  try {
    const bmp = await createImageBitmap(file);
    const size = { width: bmp.width, height: bmp.height };
    bmp.close();
    return size;
  } catch {
    return null;
  }
}

/** A friendly reason the image's size cannot be a checkout logo; run after logoFileProblem, which caps the bytes. */
export async function logoSizeProblem(file: Blob): Promise<string | null> {
  const size = await measure(file);
  if (!size) return null;
  const { width, height } = size;
  if (width < LOGO_MIN_SIDE || height < LOGO_MIN_SIDE) return `The logo is ${width} × ${height} pixels. It must be at least 32 × 32.`;
  if (width > LOGO_MAX_SIDE || height > LOGO_MAX_SIDE || width * height > LOGO_MAX_PIXELS) {
    return `The logo is ${width} × ${height} pixels, larger than we accept (4096 pixels on a side, 4 megapixels in all). Resize it to about 640 × 160.`;
  }
  return null;
}

/** The file's bytes as standard base64, with no data: prefix. */
export function readBase64(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onerror = () => reject(new Error('Could not read that file.'));
    r.onload = () => {
      const s = typeof r.result === 'string' ? r.result : '';
      resolve(s.slice(s.indexOf(',') + 1));
    };
    r.readAsDataURL(file);
  });
}

/** The logo's address: logo_url is relative to the API origin, which the page may not share. */
export function logoSrc(logoUrl: string): string {
  return `${apiOrigin()}${logoUrl}`;
}
