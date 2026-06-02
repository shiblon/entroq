import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { EntroQClient } from "./client";
import { EntroQWorker, EntroQRetryError, EntroQMoveError, EntroQStopWorker } from "./worker";
import type { Task, Doc } from "./types";

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: "t1", version: 1, queue: "q1",
    atMs: "0", claimantId: "c1", value: null,
    createdMs: "0", modifiedMs: "0",
    claims: 1, attempt: 0, err: "",
    ...overrides,
  };
}

function makeDoc(overrides: Partial<Doc> = {}): Doc {
  return {
    namespace: "ns", id: "d1", version: 1,
    claimant: "c1", atMs: "0",
    key: "k1", secondaryKey: "",
    content: null, createdMs: "0", modifiedMs: "0",
    ...overrides,
  };
}

describe("EntroQWorker", () => {
  let client: EntroQClient;
  let worker: EntroQWorker;

  beforeEach(() => {
    client = new EntroQClient({ baseUrl: "http://localhost" });
    worker = new EntroQWorker(client, { leaseMs: 1000, pollMs: 100 });
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("should claim and dispatch a task", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    vi.spyOn(client, "modify").mockResolvedValue({ changed: [], inserted: [] });

    const runPromise = worker.run(["q1"], async (t, docs) => {
      expect(t).toEqual(task);
      expect(docs).toEqual([]);
      worker.stop();
      return { deletes: [{ id: t.id, version: t.version, queue: t.queue }] };
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(client.modify).toHaveBeenCalledWith(expect.objectContaining({
      deletes: [{ id: "t1", version: 1, queue: "q1" }],
    }));
  });

  it("should fix versions after renewal before final modify", async () => {
    const task1 = makeTask({ version: 1 });
    const task2 = makeTask({ version: 2, atMs: String(Date.now() + 2000) });

    vi.spyOn(client, "claim").mockResolvedValueOnce({ task: task1 });
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [task2] });

    const runPromise = worker.run(["q1"], async (task, _docs) => {
      // Trigger renewal (leaseMs/2 = 500ms)
      await vi.advanceTimersByTimeAsync(600);
      worker.stop();
      // Return original version 1 — fixVersions should update it to 2.
      return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    const lastArg = modifySpy.mock.calls[modifySpy.mock.calls.length - 1][0];
    expect(lastArg.deletes?.[0].version).toBe(2);
  });

  it("should handle EntroQRetryError", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    const runPromise = worker.run(["q1"], async (_t, _docs) => {
      worker.stop();
      throw new EntroQRetryError("please retry", 5000);
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(modifySpy).toHaveBeenCalledWith(expect.objectContaining({
      changes: [expect.objectContaining({
        oldId: expect.objectContaining({ id: "t1", version: 1 }),
        newData: expect.objectContaining({ queue: "q1", err: "please retry", attempt: 1 }),
      })],
    }));
  });

  it("should handle EntroQRetryError with default delay when delayMs is not set", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    // Worker configured with retryDelayMs: 7000
    worker = new EntroQWorker(client, { leaseMs: 1000, retryDelayMs: 7000 });

    const runPromise = worker.run(["q1"], async () => {
      worker.stop();
      throw new EntroQRetryError("oops");  // no delayMs
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(modifySpy).toHaveBeenCalledWith(expect.objectContaining({
      changes: [expect.objectContaining({
        newData: expect.objectContaining({
          // atMs should be approximately now + 7000ms
          atMs: expect.stringMatching(/^\d+$/),
        }),
      })],
    }));
  });

  it("should handle EntroQMoveError", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    const runPromise = worker.run(["q1"], async () => {
      worker.stop();
      throw new EntroQMoveError("move it", "q2");
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(modifySpy).toHaveBeenCalledWith(expect.objectContaining({
      changes: [expect.objectContaining({
        newData: expect.objectContaining({ queue: "q2", err: "move it" }),
      })],
    }));
  });

  it("should let claim expire when EntroQMoveError has no queue and no errQueue", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    const runPromise = worker.run(["q1"], async () => {
      worker.stop();
      throw new EntroQMoveError("no dest");  // empty queue
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(modifySpy).not.toHaveBeenCalled();
  });

  it("should handle EntroQStopWorker", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    let processed = false;
    const runPromise = worker.run(["q1"], async () => {
      processed = true;
      throw new EntroQStopWorker("done");
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(processed).toBe(true);
  });

  it("stop() unblocks a waiting claim", async () => {
    // claim never resolves — simulates a blocking long-poll
    vi.spyOn(client, "claim").mockReturnValue(new Promise(() => {}));

    let ran = false;
    const runPromise = worker.run(["q1"], async () => { ran = true; });

    await Promise.resolve();  // allow run() to reach the claim
    worker.stop();
    await runPromise;

    expect(ran).toBe(false);
  });

  it("should call finisher when doWork returns void", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    let finisherTask: Task | null = null;
    const handle = EntroQWorker.handler(async (_t, _docs) => {
      worker.stop();
    }).finisher(async (t) => {
      finisherTask = t;
    });

    const runPromise = worker.run(["q1"], handle);
    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(finisherTask).toEqual(task);
  });

  it("should call finisher even when doWork returns a modification", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    let finisherRan = false;
    const handle = EntroQWorker.handler(async (t, _docs) => {
      worker.stop();
      return { deletes: [{ id: t.id, version: t.version, queue: t.queue }] };
    }).finisher(async (_t, _docs) => {
      finisherRan = true;
    });

    const runPromise = worker.run(["q1"], handle);
    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(finisherRan).toBe(true);
  });

  it("should claim docs via selector before doWork", async () => {
    const task = makeTask();
    const doc = makeDoc();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    vi.spyOn(client, "claimDocs").mockResolvedValue({ docs: [doc] });
    vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    let receivedDocs: Doc[] = [];
    const handle = EntroQWorker.handler(async (_t, docs) => {
      receivedDocs = docs;
      worker.stop();
    }).selector(async () => [{ namespace: "ns", key: "k1" }]);

    const runPromise = worker.run(["q1"], handle);
    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(client.claimDocs).toHaveBeenCalledWith({
      claimQuery: { namespace: "ns", key: "k1", durationMs: "1000" },
    });
    expect(receivedDocs).toEqual([doc]);
  });

  it("should sort doc claims by (namespace, key) to avoid deadlock", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    const claimOrder: string[] = [];
    vi.spyOn(client, "claimDocs").mockImplementation(async (req) => {
      claimOrder.push(`${req.claimQuery.namespace}/${req.claimQuery.key}`);
      return { docs: [] };
    });
    vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    const handle = EntroQWorker.handler(async () => { worker.stop(); })
      .selector(async () => [
        { namespace: "b", key: "2" },
        { namespace: "a", key: "9" },
        { namespace: "a", key: "1" },
      ]);

    const runPromise = worker.run(["q1"], handle);
    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(claimOrder).toEqual(["a/1", "a/9", "b/2"]);
  });

  it("should renew task and claimed docs together", async () => {
    const task1 = makeTask({ version: 1 });
    const task2 = makeTask({ version: 2 });
    const doc1 = makeDoc({ version: 1 });
    const doc2 = makeDoc({ version: 2 });

    vi.spyOn(client, "claim").mockResolvedValueOnce({ task: task1 });
    vi.spyOn(client, "claimDocs").mockResolvedValue({ docs: [doc1] });
    const modifySpy = vi.spyOn(client, "modify")
      .mockResolvedValue({ changed: [task2], changedDocs: [doc2] });

    const handle = EntroQWorker.handler(async (task, _docs) => {
      await vi.advanceTimersByTimeAsync(600);  // trigger renewal
      worker.stop();
      // Return original versions — fixVersions updates them.
      return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
    }).selector(async () => [{ namespace: "ns", key: "k1" }]);

    const runPromise = worker.run(["q1"], handle);
    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    // First modify call: renewal (includes task + doc changes)
    const renewalCall = modifySpy.mock.calls[0][0];
    expect(renewalCall.changes).toHaveLength(1);
    expect(renewalCall.docChanges).toHaveLength(1);

    // Last modify call: final action with version updated to 2
    const lastCall = modifySpy.mock.calls[modifySpy.mock.calls.length - 1][0];
    expect(lastCall.deletes?.[0].version).toBe(2);
  });

  it("should move task to error queue when maxAttempts exceeded", async () => {
    const task = makeTask({ attempt: 5 });
    worker = new EntroQWorker(client, { leaseMs: 1000, maxAttempts: 5 });
    // First claim returns the task; second blocks so stop() can unblock it.
    vi.spyOn(client, "claim")
      .mockResolvedValueOnce({ task })
      .mockReturnValue(new Promise(() => {}));
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    let handlerRan = false;
    const runPromise = worker.run(["q1"], async () => { handlerRan = true; });

    // Yield to let the first iteration (claim + max-attempts check) complete.
    await Promise.resolve();
    await Promise.resolve();
    worker.stop();
    await runPromise;

    expect(handlerRan).toBe(false);
    expect(modifySpy).toHaveBeenCalledWith(expect.objectContaining({
      changes: [expect.objectContaining({
        newData: expect.objectContaining({ queue: "q1/error" }),
      })],
    }));
  });

  it("HandlerBuilder chaining is immutable", () => {
    const doWork: (t: Task, docs: Doc[]) => Promise<void> = async () => {};
    const sel = async (_t: Task): Promise<never[]> => [];
    const fin: (t: Task, docs: Doc[]) => Promise<void> = async () => {};

    const h1 = EntroQWorker.handler(doWork);
    const h2 = h1.selector(sel);
    const h3 = h2.finisher(fin);

    expect(h1._selector).toBeUndefined();
    expect(h1._finisher).toBeUndefined();
    expect(h2._selector).toBe(sel);
    expect(h2._finisher).toBeUndefined();
    expect(h3._selector).toBe(sel);
    expect(h3._finisher).toBe(fin);
  });

  it("accepts a plain function as handler (no selector or finisher)", async () => {
    const task = makeTask();
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    let ran = false;
    const runPromise = worker.run(["q1"], async (_t, _docs) => {
      ran = true;
      worker.stop();
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(ran).toBe(true);
  });
});
