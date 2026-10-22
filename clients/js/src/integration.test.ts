import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { spawn, execFileSync, type ChildProcess } from "node:child_process";
import { createServer } from "node:net";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import { claimSet, M, RetryError, Worker } from "./gateway";
import { NodeGateway } from "./node";

// Repo root, two levels up from clients/js (vitest's cwd).
const repoRoot = path.resolve(process.cwd(), "..", "..");

function goAvailable(): boolean {
  try {
    execFileSync("go", ["version"], { stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.once("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const port = (s.address() as { port: number }).port;
      s.close(() => resolve(port));
    });
  });
}

// A real eqlink work, as a child of each NodeGateway, against eqmem run by `go
// run` rather than a committed binary. Skips if Go is absent.
describe.skipIf(!goAvailable())("NodeGateway against eqmem", () => {
  let eqmem: ChildProcess;
  let binDir: string;
  let eqlink: string;
  let baseUrl: string;
  let entroq: string;

  async function rest(method: string, route: string, body?: unknown): Promise<any> {
    const resp = await fetch(`${baseUrl}${route}`, {
      method,
      headers: body ? { "Content-Type": "application/json" } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    const text = await resp.text();
    if (!resp.ok) {
      throw new Error(`${method} ${route}: ${resp.status} ${text}`);
    }
    return text ? JSON.parse(text) : {};
  }

  beforeAll(async () => {
    binDir = mkdtempSync(path.join(tmpdir(), "entroq-js-"));
    eqlink = path.join(binDir, "eqlink");
    execFileSync("go", ["build", "-o", eqlink, "./cmd/eqlink"], { cwd: repoRoot, stdio: "inherit" });

    const [httpPort, grpcPort] = [await freePort(), await freePort()];
    baseUrl = `http://127.0.0.1:${httpPort}`;
    entroq = `127.0.0.1:${grpcPort}`;
    // `go run` runs the server as a child, so detached: true gives the pair a
    // process group of their own, which afterAll signals as one.
    eqmem = spawn("go", ["run", "./cmd/eqmem", "serve", "--http_port", String(httpPort), "--port", String(grpcPort)], {
      cwd: repoRoot,
      detached: true,
      stdio: "ignore",
    });
    // Generous: `go run` compiles before it serves.
    const deadline = Date.now() + 90_000;
    for (;;) {
      try {
        if ((await fetch(`${baseUrl}/api/v0/time`)).ok) {
          break;
        }
      } catch {
        // not listening yet
      }
      if (Date.now() > deadline) {
        throw new Error("eqmem did not start");
      }
      await new Promise((r) => setTimeout(r, 200));
    }
  }, 120_000);

  afterAll(() => {
    if (eqmem?.pid) {
      try {
        process.kill(-eqmem.pid, "SIGTERM");
      } catch {
        // already gone
      }
    }
    if (binDir) {
      rmSync(binDir, { recursive: true, force: true });
    }
  });

  it("works every task once, each session holding the doc set alone", async () => {
    const prefix = `js-${Date.now()}`;
    const queue = `${prefix}/q`;
    const ns = `${prefix}/ns`;
    const n = 8;
    await rest("POST", "/api/v0/modify", {
      claimantId: "seed",
      inserts: Array.from({ length: n }, (_, i) => ({ queue, value: i })),
      docInserts: [{ namespace: ns, key: "count", content: 0 }],
    });

    const gateway = new NodeGateway({ entroq, eqlink, handleSignals: false, log: () => undefined });
    const values: number[] = [];
    let inside = 0;
    let overlap = 0;
    const worker = () =>
      new Worker(
        [queue],
        {
        take: () => [claimSet(ns, "count")],
        work: async (w) => {
          if (inside++ > 0) {
            overlap++;
          }
          await new Promise((r) => setTimeout(r, 5));
          inside--;
          const count = w.docs[0];
          return M.of(M.deleting(w.task), M.changing(count, { content: Number(count.content) + 1 }));
        },
        onSuccess: (w) => {
          values.push(Number(w.value));
          if (values.length === n) {
            gateway.stop();
          }
        },
      },
      // A session that finds the set held retries after this, not the
      // worker's default of 30s.
      { retryDelayS: 1 },
    );
    await gateway.run(worker(), worker(), worker());

    expect(values.sort((a, b) => a - b)).toEqual(Array.from({ length: n }, (_, i) => i));
    expect(overlap).toBe(0);
    const left = await rest("GET", `/api/v0/tasks?queue=${encodeURIComponent(queue)}`);
    expect(left.tasks ?? []).toEqual([]);
    const docs = await rest("GET", `/api/v0/docs?query.namespace=${encodeURIComponent(ns)}`);
    expect(docs.docs.map((d: any) => d.content)).toEqual([n]);
  }, 60_000);

  it("retries a task its work throws RetryError for, counting the attempt", async () => {
    const queue = `js-${Date.now()}/retry`;
    await rest("POST", "/api/v0/modify", { claimantId: "seed", inserts: [{ queue, value: "x" }] });

    const gateway = new NodeGateway({ entroq, eqlink, handleSignals: false, log: () => undefined });
    const attempts: number[] = [];
    await gateway.run(
      new Worker(
        [queue],
        {
          work: (w, ctx) => {
            attempts.push(w.task.attempt ?? 0);
            if (attempts.length === 1) {
              throw new RetryError("not yet");
            }
            ctx.stop();
            return M.of(M.deleting(w.task));
          },
        },
        { retryDelayS: 1 },
      ),
    );

    expect(attempts).toEqual([0, 1]);
    const left = await rest("GET", `/api/v0/tasks?queue=${encodeURIComponent(queue)}`);
    expect(left.tasks ?? []).toEqual([]);
  }, 60_000);
});
