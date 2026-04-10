import { describe, it, expect, beforeAll, afterAll, beforeEach } from "vitest";
import { EntroQPG } from "./index";
import { EntroQWorker } from "../worker";

// Integration test for direct-to-Postgres client.
// Requires PGHOST, PGPORT, PGUSER, PGDATABASE, PGPASSWORD environment variables.
// Skips if PGHOST is not set.
const runPGTests = !!process.env.PGHOST;

describe("EntroQ PG Integration", () => {
  if (!runPGTests) {
    it.skip("PG Integration tests (PGHOST not set)", () => {});
    return;
  }

  let client: EntroQPG;
  const config = {
      host: process.env.PGHOST,
      port: parseInt(process.env.PGPORT || "5432"),
      user: process.env.PGUSER,
      password: process.env.PGPASSWORD,
      database: process.env.PGDATABASE,
  };

  beforeAll(async () => {
    client = new EntroQPG(config);
    await client.connect();
  });

  afterAll(async () => {
    if (client) {
      await client.disconnect();
    }
  });

  beforeEach(async () => {
      // Clear tasks before each test
      const pg = (client as any).client;
      await pg.query("TRUNCATE entroq.tasks");
      await pg.query("DELETE FROM entroq.notification_state");
      await pg.query("INSERT INTO entroq.notification_state (id, last_at) VALUES (1, now())");
  });

  it("should perform basic operations directly against Postgres", async () => {
    const time = await client.time();
    expect(Number(time)).toBeGreaterThan(0);

    const q = "/test/pg-integration";
    
    // 1. Insert
    const modResp = await client.modify({
      inserts: [{ queue: q, atMs: "0", value: { msg: "hello-pg" } }]
    });
    expect(modResp.inserted).toHaveLength(1);
    const task = modResp.inserted![0];

    // 2. Tasks list
    const tasksResp = await client.tasks(q);
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
    const finalTasks = await client.tasks(q);
    expect(finalTasks.tasks).toHaveLength(0);
  });

  it("should wake up immediately via LISTEN/NOTIFY", async () => {
      const q = "/test/pg-notify";
      
      const claimPromise = client.claim([q], 30000, 10000); // 10s poll, but should wake immediately
      
      const start = Date.now();
      // Wait a tiny bit to ensure the client is LISTENing
      await new Promise(r => setTimeout(r, 500));
      
      await client.modify({
          inserts: [{ queue: q, atMs: "0", value: "notified" }]
      });
      
      const claimResp = await claimPromise;
      const elapsed = Date.now() - start;
      
      expect(claimResp.task).toBeDefined();
      expect(claimResp.task?.value).toBe("notified");
      // If LISTEN/NOTIFY is working, it should take much less than the 10s poll interval.
      expect(elapsed).toBeLessThan(3000); 
  });

  it("should work with EntroQWorker against Postgres", async () => {
    const q = "/test/pg-worker";
    await client.modify({
      inserts: [{ queue: q, atMs: "0", value: "pg-work1" }]
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
    expect(handledValue).toBe("pg-work1");

    const tasks = await client.tasks(q);
    expect(tasks.tasks).toHaveLength(0);
  });
});
