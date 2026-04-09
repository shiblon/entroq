import { describe, it, expect, vi, beforeEach } from "vitest";
import { EntroQClient } from "./client";

describe("EntroQClient", () => {
  const baseUrl = "http://localhost:9100";
  let client: EntroQClient;

  beforeEach(() => {
    client = new EntroQClient({ baseUrl });
    vi.stubGlobal("fetch", vi.fn());
  });

  it("should generate a consistent claimant ID", () => {
    const cid = client.getClaimantId();
    expect(cid).toHaveLength(16);
    expect(cid).toMatch(/^[0-9a-f]{16}$/);
    expect(client.getClaimantId()).toBe(cid);
  });

  it("should handle tryClaim correctly", async () => {
    const mockTask = {
      id: "task-1",
      version: 1,
      queue: "q1",
      atMs: "1000",
      claimantId: "cid-1",
      value: { data: "hello" },
      createdMs: "1000",
      modifiedMs: "1000",
      claims: 1,
      attempt: 1,
      err: "",
    };

    vi.mocked(fetch).mockResolvedValueOnce({
      ok: true,
      json: async () => ({ task: mockTask }),
    } as Response);

    const resp = await client.tryClaim(["q1"], 10000);
    expect(resp.task).toEqual(mockTask);

    expect(fetch).toHaveBeenCalledWith(`${baseUrl}/api/v0/claim`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        claimantId: client.getClaimantId(),
        queues: ["q1"],
        durationMs: "10000",
        pollMs: "0",
      }),
    });
  });

  it("should handle modify correctly", async () => {
    const mockResp = {
      inserted: [{ id: "new-task", version: 1, queue: "q1" }],
    };

    vi.mocked(fetch).mockResolvedValueOnce({
      ok: true,
      json: async () => mockResp,
    } as Response);

    const resp = await client.modify({
      inserts: [{ queue: "q1", atMs: "0", value: "val" }],
    });
    expect(resp).toEqual(mockResp);

    expect(fetch).toHaveBeenCalledWith(`${baseUrl}/api/v0/modify`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        claimantId: client.getClaimantId(),
        inserts: [{ queue: "q1", atMs: "0", value: "val" }],
      }),
    });
  });

  it("should throw error on failed request", async () => {
    vi.mocked(fetch).mockResolvedValueOnce({
      ok: false,
      status: 403,
      text: async () => "Forbidden",
    } as Response);

    await expect(client.tryClaim(["q1"])).rejects.toThrow(
      "EntroQ request failed (403): Forbidden"
    );
  });

  it("should handle JSON error messages in failed requests", async () => {
    vi.mocked(fetch).mockResolvedValueOnce({
      ok: false,
      status: 400,
      text: async () => JSON.stringify({ message: "Bad Queue Name" }),
    } as Response);

    await expect(client.tryClaim(["q1"])).rejects.toThrow(
      "EntroQ request failed (400): Bad Queue Name"
    );
  });
});
