import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const read = (p: string) => readFileSync(join(process.cwd(), p), 'utf8');
const shape = (svg: string) => [...svg.matchAll(/<path d="([^"]+)"[^>]*stroke-width="([^"]+)"/g)].map((m) => `${m[1]} @${m[2]}`);

describe('brand mark', () => {
  it('draws the same prism as the apps, in the header and the favicon', () => {
    const mark = shape(read('../checkout/public/brand/prismaport-mark-white.svg'));
    expect(mark).toHaveLength(3);
    const header = /<svg class="mark"[\s\S]*?<\/svg>/.exec(read('index.html'))?.[0] ?? '';
    expect(shape(header)).toEqual(mark);
    expect(read('public/favicon.svg')).toBe(read('../checkout/public/favicon.svg'));
  });
});
