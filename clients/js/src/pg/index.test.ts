import { describe, it, expect, vi, beforeEach } from "vitest";
import { EntroQPG, pgChannelName } from "./index";
import { Client } from "pg";

// Mock the pg Client
vi.mock("pg", () => {
  const Client = vi.fn();
  Client.prototype.connect = vi.fn();
  Client.prototype.end = vi.fn();
  Client.prototype.query = vi.fn();
  Client.prototype.on = vi.fn();
  Client.prototype.once = vi.fn();
  Client.prototype.removeListener = vi.fn();
  return { Client };
});

describe("EntroQPG", () => {
  let client: EntroQPG;
  let mockPg: any;

  beforeEach(() => {
    vi.clearAllMocks();
    client = new EntroQPG({ connectionString: "test" }, "test-claimant");
    // Access the mocked internal client
    mockPg = (client as any).client;
  });

  it("should verify schema version on connect", async () => {
    mockPg.query.mockResolvedValueOnce({ 
        rowCount: 1, 
        rows: [{ value: "0.10.0" }] 
    });

    await client.connect();

    expect(mockPg.connect).toHaveBeenCalled();
    expect(mockPg.query).toHaveBeenCalledWith(
      "SELECT value FROM entroq.meta WHERE key = 'schema_version'"
    );
  });

  it("should throw error on version mismatch", async () => {
    mockPg.query.mockResolvedValueOnce({ 
        rowCount: 1, 
        rows: [{ value: "0.0.1" }] 
    });

    await expect(client.connect()).rejects.toThrow(/schema version mismatch/);
  });

  it("should call the modify stored procedure with correct JSON", async () => {
    mockPg.query.mockResolvedValueOnce({ rows: [] });

    await client.modify({
      inserts: [{ queue: "q1", atMs: "0", value: { foo: "bar" } }],
      deletes: [{ id: "t1", version: 1, queue: "q1" }]
    });

    expect(mockPg.query).toHaveBeenCalledWith(
      expect.stringContaining("SELECT * FROM entroq.modify"),
      [
        "test-claimant",
        "[]", // depends
        JSON.stringify([{ id: "t1", version: 1, queue: "q1" }]), // deletes
        JSON.stringify([{ queue: "q1", atMs: "0", value: { foo: "bar" } }]), // inserts
        "[]" // changes
      ]
    );
  });

  it("should implement tryClaim correctly", async () => {
      mockPg.query.mockResolvedValueOnce({
          rows: [{
              id: "t1",
              version: 1,
              queue: "q1",
              at: new Date().toISOString(),
              claimant: "test-claimant",
              value: { work: true },
              created: new Date().toISOString(),
              modified: new Date().toISOString(),
              claims: 1,
              attempt: 0,
              err: ""
          }]
      });

      const task = await client.tryClaim("q1", 10000);
      
      expect(task).toBeDefined();
      expect(task?.id).toBe("t1");
      expect(mockPg.query).toHaveBeenCalledWith(
          "SELECT * FROM entroq.try_claim($1, $2, $3::interval)",
          [["q1"], "test-claimant", "10000 milliseconds"]
      );
  });
});

describe("pgChannelName", () => {
    it("should sanitize short names simple", () => {
        expect(pgChannelName("foo")).toBe("q_foo");
    });

    it("should use the MD5 sandwich for very long names", () => {
        const longName = "a".repeat(100);
        const name = pgChannelName(longName);
        expect(name.length).toBeLessThanOrEqual(63);
        expect(name).toMatch(/^q_a{25}_[a-f0-9]{8}_a{26}$/);
    });
});
