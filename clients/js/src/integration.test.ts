import { describe, it, expect, beforeAll, afterAll, beforeEach } from "vitest";
import { EntroQClient, EntroQDependencyError } from "./client";
import { EntroQWorker } from "./worker";
import { spawn, execFileSync, ChildProcess } from "child_process";
import path from "path";

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

// Run eqmem via `go run` rather than committing a binary. Skips if Go is absent.
describe.skipIf(!goAvailable())("EntroQ Integration", () => {
  let eqmemsvc: ChildProcess;
  let client: EntroQClient;
  const httpPort = 9101; // Use a different port than default
  const grpcPort = 37707;
  const baseUrl = `http://localhost:${httpPort}`;

  beforeAll(async () => {
    return new Promise((resolve, reject) => {
      // `go run` runs the compiled server as a child, so a plain SIGTERM to the
      // `go run` process would orphan it. detached: true puts both in their own
      // process group; afterAll signals the whole group to reap them together.
      eqmemsvc = spawn("go", [
        "run", "./cmd/eqmem", "serve",
        "--http_port", httpPort.toString(),
        "--port", grpcPort.toString()
      ], { cwd: repoRoot, detached: true });

      const onData = (data: Buffer) => {
        if (data.toString().includes("Starting EntroQ server")) {
          // Wait a tiny bit for the HTTP server to also be ready
          setTimeout(resolve, 500);
        }
      };

      eqmemsvc.stdout?.on("data", onData);
      eqmemsvc.stderr?.on("data", onData);

      eqmemsvc.on("error", reject);

      // Generous: `go run` compiles before it serves (cold cache on CI).
      setTimeout(() => reject(new Error("Timeout waiting for eqmemsvc")), 60000);
    });
  });

  afterAll(() => {
    if (eqmemsvc?.pid) {
      // Negative pid signals the whole process group (go run + the server).
      try { process.kill(-eqmemsvc.pid, "SIGTERM"); } catch { /* already gone */ }
    }
  });

  beforeEach(() => {
    client = new EntroQClient({ baseUrl });
  });

  it("should perform basic operations against a real server", async () => {
    const time = await client.time();
    expect(Number(time)).toBeGreaterThan(0);

    const q = "/test/integration";
    
    // 1. Insert
    const modResp = await client.modify({
      inserts: [{ queue: q, atMs: "0", value: { msg: "hello" } }]
    });
    expect(modResp.inserted).toHaveLength(1);
    const task = modResp.inserted![0];

    // 2. Tasks list
    const tasksResp = await client.tasks({ queue: q });
    expect(tasksResp.tasks).toHaveLength(1);
    expect(tasksResp.tasks[0].id).toBe(task.id);

    // 3. Claim
    const claimResp = await client.claim([q]);
    expect(claimResp.task).toBeDefined();
    expect(claimResp.task?.id).toBe(task.id);

    // 4. Delete
    await client.modify({
      deletes: [{ id: task.id, version: claimResp.task!.version, queue: q }]
    });

    // 5. Verify empty
    const finalTasks = await client.tasks({ queue: q });
    expect(finalTasks.tasks).toHaveLength(0);
  });

  it("should surface a dependency error (409) as EntroQDependencyError", async () => {
    const q = "/test/dep-error";
    const ins = await client.modify({
      inserts: [{ queue: q, atMs: "0", value: "v" }]
    });
    const task = ins.inserted![0];

    // Delete at a version that does not exist: an optimistic-concurrency
    // conflict the server reports as 409 with flat ModifyDep details.
    const stale = { id: task.id, version: 999, queue: q };
    await expect(
      client.modify({ deletes: [stale] })
    ).rejects.toBeInstanceOf(EntroQDependencyError);

    try {
      await client.modify({ deletes: [stale] });
    } catch (err) {
      const dep = err as EntroQDependencyError;
      expect(dep.deletes).toHaveLength(1);
      expect(dep.deletes[0].id).toBe(task.id);
    }
  });

  it("should work with EntroQWorker against a real server", async () => {
    const q = "/test/worker";
    await client.modify({
      inserts: [{ queue: q, atMs: "0", value: "work1" }]
    });

    const worker = new EntroQWorker(client, { leaseMs: 1000, pollMs: 100 });
    
    let handledValue: any;
    const workerPromise = worker.run([q], async (task) => {
      handledValue = task.value;
      worker.stop();
      return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
    });

    await workerPromise;
    expect(handledValue).toBe("work1");

    const tasks = await client.tasks({ queue: q });
    expect(tasks.tasks).toHaveLength(0);
  });

  it("should support streaming tasks", async () => {
    const q = "/test/stream";
    
    // 1. Insert tasks first (current eqmemsvc StreamTasks returns currently available tasks and closes)
    await client.modify({
        inserts: [
            { queue: q, atMs: "0", value: "s1" },
            { queue: q, atMs: "0", value: "s2" }
        ]
    });

    // 2. Start streaming and collect results
    const foundTasks: any[] = [];
    for await (const task of client.streamTasks({ queue: q })) {
        foundTasks.push(task);
        if (foundTasks.length === 2) break;
    }

    expect(foundTasks).toHaveLength(2);
    expect(foundTasks.map(t => t.value)).toContain("s1");
    expect(foundTasks.map(t => t.value)).toContain("s2");
  });
});
