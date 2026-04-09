import { describe, it, expect, beforeAll, afterAll, beforeEach } from "vitest";
import { EntroQClient } from "./client";
import { EntroQWorker } from "./worker";
import { spawn, ChildProcess } from "child_process";
import path from "path";

describe("EntroQ Integration", () => {
  let eqmemsvc: ChildProcess;
  let client: EntroQClient;
  const httpPort = 9101; // Use a different port than default
  const grpcPort = 37707;
  const baseUrl = `http://localhost:${httpPort}`;

  beforeAll(async () => {
    return new Promise((resolve, reject) => {
      const binPath = path.join(process.cwd(), "eqmemsvc");
      eqmemsvc = spawn(binPath, [
        "--http_port", httpPort.toString(),
        "--port", grpcPort.toString()
      ]);

      const onData = (data: Buffer) => {
        if (data.toString().includes("Starting EntroQ server")) {
          // Wait a tiny bit for the HTTP server to also be ready
          setTimeout(resolve, 500);
        }
      };

      eqmemsvc.stdout?.on("data", onData);
      eqmemsvc.stderr?.on("data", onData);

      eqmemsvc.on("error", reject);
      
      // Safety timeout
      setTimeout(() => reject(new Error("Timeout waiting for eqmemsvc")), 5000);
    });
  });

  afterAll(() => {
    if (eqmemsvc) {
      eqmemsvc.kill();
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

  it("should work with EntroQWorker against a real server", async () => {
    const q = "/test/worker";
    await client.modify({
      inserts: [{ queue: q, atMs: "0", value: "work1" }]
    });

    const worker = new EntroQWorker(client, { leaseMs: 1000, pollMs: 100 });
    
    let handledValue: any;
    const workerPromise = worker.run([q], async (task, stop) => {
      handledValue = task.value;
      await stop();
      worker.stop();
      return "delete";
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
