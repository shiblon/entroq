import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { EntroQClient } from "./client";
import { EntroQWorker, EntroQRetryError, EntroQMoveError } from "./worker";

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
    const task = { id: "t1", version: 1, queue: "q1", atMs: "0", claimantId: "c1", value: "v1", createdMs: "0", modifiedMs: "0", claims: 1, attempt: 1, err: "" };
    
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    vi.spyOn(client, "modify").mockResolvedValue({ changed: [], inserted: [] });

    // We only want to run one loop iteration.
    let handlerCalled = false;
    const runPromise = worker.run(["q1"], async (t, stop) => {
      handlerCalled = true;
      expect(t).toEqual(task);
      const stable = await stop();
      expect(stable).toEqual(task);
      worker.stop();
      return "delete";
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(handlerCalled).toBe(true);
    expect(client.claim).toHaveBeenCalledWith(["q1"], 1000, 100);
    expect(client.modify).toHaveBeenCalledWith({ deletes: [{ id: "t1", version: 1, queue: "q1" }] });
  });

  it("should automatically renew a task", async () => {
    const task1 = { id: "t1", version: 1, queue: "q1", atMs: String(Date.now() + 1000), claimantId: "c1", value: "v1", createdMs: "0", modifiedMs: "0", claims: 1, attempt: 1, err: "" };
    const task2 = { ...task1, version: 2, atMs: String(Date.now() + 2000) };

    vi.spyOn(client, "claim").mockResolvedValueOnce({ task: task1 });
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [task2] });

    let stableTask: any;
    const runPromise = worker.run(["q1"], async (task, stop) => {
      // Advance time to trigger renewal (leaseMs / 2 = 500ms)
      await vi.advanceTimersByTimeAsync(600);
      stableTask = await stop();
      worker.stop();
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(modifySpy).toHaveBeenCalled();
    expect(stableTask.version).toBe(2);
  });

  it("should handle EntroQRetryError", async () => {
    const task = { id: "t1", version: 1, queue: "q1", atMs: "0", claimantId: "c1", value: "v1", createdMs: "0", modifiedMs: "0", claims: 1, attempt: 1, err: "" };
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    const runPromise = worker.run(["q1"], async () => {
      worker.stop();
      throw new EntroQRetryError("please retry", 5000);
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(modifySpy).toHaveBeenCalledWith({
      changes: [{
        oldId: { id: "t1", version: 1, queue: "q1" },
        newData: expect.objectContaining({
          queue: "q1",
          err: "please retry",
          attempt: 2,
          value: "v1",
        }),
      }],
    });
  });

  it("should handle EntroQMoveError", async () => {
    const task = { id: "t1", version: 1, queue: "q1", atMs: "0", claimantId: "c1", value: "v1", createdMs: "0", modifiedMs: "0", claims: 1, attempt: 1, err: "" };
    vi.spyOn(client, "claim").mockResolvedValueOnce({ task });
    const modifySpy = vi.spyOn(client, "modify").mockResolvedValue({ changed: [] });

    const runPromise = worker.run(["q1"], async () => {
      worker.stop();
      throw new EntroQMoveError("move it", "q2");
    });

    await vi.runOnlyPendingTimersAsync();
    await runPromise;

    expect(modifySpy).toHaveBeenCalledWith({
      changes: [{
        oldId: { id: "t1", version: 1, queue: "q1" },
        newData: expect.objectContaining({
          queue: "q2",
          err: "move it",
        }),
      }],
    });
  });
});
