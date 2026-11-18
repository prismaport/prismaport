import { apiBase, exampleFor, type Operation } from './spec';

export type Lang = 'curl' | 'node' | 'go' | 'python';

export const LANGS: [Lang, string][] = [
  ['curl', 'cURL'],
  ['node', 'Node'],
  ['go', 'Go'],
  ['python', 'Python'],
];

export const exampleKey = (live: boolean): string => (live ? 'sk_live_4fGh…' : 'sk_test_8pQw…');

function url(op: Operation, live: boolean): string {
  let path = op.path;
  for (const p of op.params.filter((x) => x.in === 'path')) {
    if (typeof p.example === 'string') path = path.replace(`{${p.name}}`, p.example);
  }
  const query = op.params
    .filter((p) => p.in === 'query' && p.example !== undefined)
    .map((p) => `${p.name}=${encodeURIComponent(String(exampleFor(op, p.example, live)))}`);
  return apiBase + path + (query.length ? '?' + query.join('&') : '');
}

/** A request for op in lang, with the example body and the headers the endpoint needs. */
export function snippet(op: Operation, lang: Lang, live: boolean): string {
  const target = url(op, live);
  const body = op.body?.example === undefined ? null : exampleFor(op, op.body.example, live);
  const json = body === null ? '' : JSON.stringify(body, null, 2);
  const auth = !op.isPublic;
  const idemNote = op.isPublic ? 'one random value per payer attempt' : 'one stable value per order; reuse it on retries';

  if (lang === 'curl') {
    const lines = [`curl ${op.method !== 'GET' ? `-X ${op.method} ` : ''}${shellQuote(target)}`];
    if (auth) lines.push(`  -H "Authorization: Bearer ${exampleKey(live)}"`);
    if (op.idempotency) lines.push(`  -H "Idempotency-Key: ${op.isPublic ? '7d1c4e9a-2b8f-4c3d-9e1a-5f6b7c8d9e0f' : 'order-42-attempt-1'}"`);
    if (body !== null) {
      lines.push('  -H "Content-Type: application/json"');
      lines.push(`  -d ${shellQuote(JSON.stringify(body))}`);
    }
    return lines.join(' \\\n');
  }
  if (lang === 'node') {
    const headers: string[] = [];
    if (auth) headers.push('    Authorization: `Bearer ${process.env.PRISMAPORT_SECRET_KEY}`,');
    if (op.idempotency) headers.push(`    'Idempotency-Key': ${op.isPublic ? 'attemptId' : '`${orderId}-attempt-1`'}, // ${idemNote}`);
    if (body !== null) headers.push("    'Content-Type': 'application/json',");
    return [
      `const res = await fetch('${target}', {`,
      `  method: '${op.method}',`,
      headers.length ? `  headers: {\n${headers.join('\n')}\n  },` : null,
      body !== null ? `  body: JSON.stringify(${json.replace(/\n/g, '\n  ')}),` : null,
      '});',
      'if (!res.ok) throw new Error((await res.json()).error.code);',
      'const data = await res.json();',
    ]
      .filter((l): l is string => l !== null)
      .join('\n');
  }
  if (lang === 'go') {
    const out: string[] = [];
    if (body !== null) out.push(`body := strings.NewReader(\`${JSON.stringify(body)}\`)`);
    out.push(`req, err := http.NewRequestWithContext(ctx, "${op.method}", "${target}", ${body !== null ? 'body' : 'nil'})`, 'if err != nil {', '\treturn err', '}');
    if (auth) out.push('req.Header.Set("Authorization", "Bearer "+os.Getenv("PRISMAPORT_SECRET_KEY"))');
    if (op.idempotency) out.push(`req.Header.Set("Idempotency-Key", ${op.isPublic ? 'attemptID' : 'orderID+"-attempt-1"'}) // ${idemNote}`);
    if (body !== null) out.push('req.Header.Set("Content-Type", "application/json")');
    out.push('res, err := http.DefaultClient.Do(req)', 'if err != nil {', '\treturn err', '}', 'defer res.Body.Close()');
    return out.join('\n');
  }
  const headers: string[] = [];
  if (auth) headers.push(`"Authorization": f"Bearer {os.environ['PRISMAPORT_SECRET_KEY']}"`);
  if (op.idempotency) headers.push(op.isPublic ? '"Idempotency-Key": attempt_id' : '"Idempotency-Key": f"{order_id}-attempt-1"');
  return [
    op.idempotency ? `# Idempotency-Key: ${idemNote}` : null,
    `res = requests.${op.method.toLowerCase()}(`,
    `    "${target}",`,
    headers.length ? `    headers={${headers.join(', ')}},` : null,
    body !== null ? `    json=${pythonLiteral(json).replace(/\n/g, '\n    ')},` : null,
    '    timeout=10,',
    ')',
    'res.raise_for_status()',
  ]
    .filter((l): l is string => l !== null)
    .join('\n');
}

/** Single-quotes a shell word, so ?, & and spaces are literal in bash and zsh. */
export function shellQuote(s: string): string {
  return `'${s.replaceAll("'", `'\\''`)}'`;
}

/** JSON text as a Python literal: the only differences in our examples are true, false and null. */
export function pythonLiteral(json: string): string {
  return json.replace(/("(?:[^"\\]|\\.)*")|\btrue\b|\bfalse\b|\bnull\b/g, (m, str: string | undefined) =>
    str ? str : m === 'true' ? 'True' : m === 'false' ? 'False' : 'None',
  );
}
