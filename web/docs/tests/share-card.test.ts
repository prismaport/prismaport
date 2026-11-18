import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const read = (p: string) => readFileSync(join(process.cwd(), p));
const meta = (html: string, attr: string, name: string) =>
  new RegExp(`<meta ${attr}="${name}" content="([^"]*)"`).exec(html)?.[1];

describe('share card', () => {
  const html = read('index.html').toString('utf8');

  it('points link previews at the portal card', () => {
    expect(meta(html, 'property', 'og:url')).toBe('https://docs.prismaport.com/');
    expect(meta(html, 'property', 'og:image')).toBe('https://docs.prismaport.com/og.png');
    expect(meta(html, 'name', 'twitter:image')).toBe(meta(html, 'property', 'og:image'));
    expect(meta(html, 'name', 'twitter:card')).toBe('summary_large_image');
  });

  it('ships public/og.png as a 1200 x 630 PNG', () => {
    const png = read('public/og.png');
    expect(png.subarray(1, 4).toString('latin1')).toBe('PNG');
    expect([png.readUInt32BE(16), png.readUInt32BE(20)]).toEqual([1200, 630]);
  });

  // An edit to og/og.svg has to be rendered too: task web:landing:og.
  it('was rendered from the current og/og.svg', () => {
    const sum = createHash('sha256').update(read('og/og.svg')).digest('hex');
    expect(read('og/og.svg.sha256').toString('utf8').trim()).toBe(sum);
  });
});
