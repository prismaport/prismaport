// Webhook handlers with no Prismaport package: the same rules as internal/webhooksig, in each language's
// standard library. tests/webhook-handlers.test.ts runs the Node verify against the shared vectors.

export type HookLang = 'node' | 'python' | 'go' | 'php';

export const HOOK_LANGS: [HookLang, string][] = [
  ['node', 'Node'],
  ['python', 'Python'],
  ['go', 'Go'],
  ['php', 'PHP'],
];

/** The Node verify on its own (it uses createHmac and timingSafeEqual from node:crypto). */
export const NODE_VERIFY = `const TOLERANCE = 300; // seconds either side of your clock

// True when one v1 in the header is HMAC-SHA256(secret, t + "." + raw body).
// During a rotation the header has one v1 per secret: any match passes.
function verify(rawBody, header, secret, now = Date.now() / 1000) {
  let t = -1;
  const sigs = [];
  for (const part of (header ?? '').split(',')) {
    const p = part.trim();
    const i = p.indexOf('=');
    if (i < 0) return false;
    const [k, v] = [p.slice(0, i), p.slice(i + 1)];
    if (k === 't') {
      if (t !== -1 || !/^\\d+$/.test(v)) return false;
      t = Number(v);
    } else if (k === 'v1') sigs.push(v);
  }
  if (t < 0 || sigs.length === 0 || Math.abs(now - t) > TOLERANCE) return false;
  const want = Buffer.from(createHmac('sha256', secret).update(\`\${t}.\`).update(rawBody).digest('hex'));
  return sigs.some((s) => {
    const got = Buffer.from(s);
    return got.length === want.length && timingSafeEqual(got, want);
  });
}`;

const NODE = `import { createHmac, timingSafeEqual } from 'node:crypto';
import express from 'express';

${NODE_VERIFY}

const app = express();

// express.raw keeps the bytes we signed. A JSON parser in front of this route breaks the check.
app.post('/webhooks', express.raw({ type: 'application/json' }), (req, res) => {
  if (!verify(req.body, req.get('Signature'), process.env.PRISMAPORT_WEBHOOK_SECRET)) return res.sendStatus(400);
  const event = JSON.parse(req.body);
  res.sendStatus(204);
  // Dedupe by event.id, skip versions you already applied,
  // then read GET /v1/sessions/{id} before you grant anything.
});

app.listen(3000);`;

const PYTHON = `import hashlib
import hmac
import json
import os
import time

from flask import Flask, request

TOLERANCE = 300  # seconds either side of your clock


def verify(raw_body: bytes, header: str, secret: str, now: float | None = None) -> bool:
    """True when one v1 in the header is HMAC-SHA256(secret, t + "." + raw body).

    During a rotation the header has one v1 per secret: any match passes."""
    t, sigs = -1, []
    for part in (header or "").split(","):
        k, sep, v = part.strip().partition("=")
        if not sep:
            return False
        if k == "t":
            if t != -1 or not (v.isascii() and v.isdigit()):
                return False
            t = int(v)
        elif k == "v1":
            sigs.append(v.encode())
    now = time.time() if now is None else now
    if t < 0 or not sigs or abs(now - t) > TOLERANCE:
        return False
    want = hmac.new(secret.encode(), f"{t}.".encode() + raw_body, hashlib.sha256).hexdigest().encode()
    return any(hmac.compare_digest(s, want) for s in sigs)


app = Flask(__name__)


@app.post("/webhooks")
def webhooks():
    raw = request.get_data()  # the bytes we signed, before any JSON parsing
    if not verify(raw, request.headers.get("Signature", ""), os.environ["PRISMAPORT_WEBHOOK_SECRET"]):
        return "", 400
    event = json.loads(raw)
    # Dedupe by event["id"], skip versions you already applied, queue the work
    # and answer within 10 seconds. Read GET /v1/sessions/{id} before you grant anything.
    return "", 204`;

const GO = `package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const tolerance = 5 * time.Minute // either side of your clock

// verify reports whether one v1 in header is HMAC-SHA256(secret, t + "." + body).
// During a rotation the header has one v1 per secret: any match passes.
func verify(body []byte, header, secret string, now time.Time) bool {
	t, sigs := int64(-1), []string{}
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return false
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || t != -1 {
				return false
			}
			t = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if t < 0 || len(sigs) == 0 {
		return false
	}
	if d := now.Sub(time.Unix(t, 0)); d > tolerance || d < -tolerance {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(t, 10) + "."))
	m.Write(body)
	want := []byte(hex.EncodeToString(m.Sum(nil)))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), want) {
			return true
		}
	}
	return false
}

func main() {
	secret := os.Getenv("PRISMAPORT_WEBHOOK_SECRET")
	http.HandleFunc("POST /webhooks", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body) // the bytes we signed
		if err != nil || !verify(body, r.Header.Get("Signature"), secret, time.Now()) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var event struct {
			ID   string \`json:"id"\`
			Type string \`json:"type"\`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		// Dedupe by event.ID, skip versions you already applied,
		// then read GET /v1/sessions/{id} before you grant anything.
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}`;

const PHP = `<?php
const TOLERANCE = 300; // seconds either side of your clock

// True when one v1 in the header is HMAC-SHA256(secret, t + "." + raw body).
// During a rotation the header has one v1 per secret: any match passes.
function verify(string $rawBody, string $header, string $secret, ?int $now = null): bool
{
    $t = -1;
    $sigs = [];
    foreach (explode(',', $header) as $part) {
        $kv = explode('=', trim($part), 2);
        if (count($kv) !== 2) {
            return false;
        }
        [$k, $v] = $kv;
        if ($k === 't') {
            if ($t !== -1 || !ctype_digit($v)) {
                return false;
            }
            $t = (int) $v;
        } elseif ($k === 'v1') {
            $sigs[] = $v;
        }
    }
    $now ??= time();
    if ($t < 0 || !$sigs || abs($now - $t) > TOLERANCE) {
        return false;
    }
    $want = hash_hmac('sha256', $t . '.' . $rawBody, $secret);
    foreach ($sigs as $s) {
        if (hash_equals($want, $s)) {
            return true;
        }
    }
    return false;
}

$raw = file_get_contents('php://input'); // the bytes we signed
if (!verify($raw, $_SERVER['HTTP_SIGNATURE'] ?? '', getenv('PRISMAPORT_WEBHOOK_SECRET'))) {
    http_response_code(400);
    exit;
}
$event = json_decode($raw, true);
http_response_code(204);
// Dedupe by $event['id'], skip versions you already applied,
// then read GET /v1/sessions/{id} before you grant anything.`;

export const HANDLERS: Record<HookLang, string> = { node: NODE, python: PYTHON, go: GO, php: PHP };
