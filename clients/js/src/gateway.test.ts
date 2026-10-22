import { describe, it, expect } from "vitest";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import {
  claimSet,
  FatalWorker,
  Gateway,
  GatewayError,
  HTTPTransport,
  M,
  type Message,
  MoveError,
  RetryError,
  Session,
  SessionGone,
  type Transport,
  Worker,
} from "./gateway";
import type { Doc, Task } from "./types";

const task: Task = {
  queue: "in",
  id: "t1",
  version: 3,
  atMs: "0",
  claimantId: "c",
  value: { n: 1 },
  createdMs: "0",
  modifiedMs: "0",
  claims: 1,
  attempt: 0,
  err: "",
};

const doc: Doc = {
  namespace: "ns",
  id: "d1",
  version: 2,
  claimant: "",
  atMs: "0",
  key: "k",
  secondaryKey: "s",
  content: { a: 1 },
  createdMs: "0",
  modifiedMs: "0",
};

/**
 * A scripted gateway: answer(msg, sent) returns the reply to each message, and
 * sent records every message the client sent, in order.
 */
class FakeGateway implements Transport {
  readonly sent: Message[] = [];
  closed = false;

  constructor(private readonly answer: (msg: Message, sent: Message[]) => Message | Promise<Message>) {}

  async exchange(msg: Message): Promise<Message> {
    this.sent.push(msg);
    const reply = await this.answer(msg, this.sent);
    if (reply.type === "error") {
      throw reply.class === "gone" ? new SessionGone(reply.message) : new GatewayError(reply.class, reply.message);
    }
    return reply;
  }

  close(): void {
    this.closed = true;
  }
}

/** A gateway that hands out one task, then answers the next request with quit. */
function oneTask(opts: { docs?: boolean; success?: boolean; silenceS?: number } = {}) {
  return new FakeGateway((msg) => {
    switch (msg.type) {
      case "config":
        return { type: "ready", session: "s1", silence_timeout_s: opts.silenceS ?? 0 };
      case "ready":
        return opts.docs ? { type: "take", task } : { type: "work", task, doc_sets: [] };
      case "take":
        return { type: "work", task, doc_sets: [{ set: { ...doc, id: "", content: undefined }, docs: [doc] }] };
      case "modify":
        return opts.success && !msg.outcome ? { type: "success" } : { type: "quit" };
      case "working":
        return { type: "ok" };
      default:
        return { type: "quit" };
    }
  });
}

describe("Session", () => {
  it("registers, works a task, and commits what work returns", async () => {
    const gw = oneTask();
    const w = new Worker(["in"], { work: (w) => M.of(M.deleting(w.task)) }, { maxAttempts: 3, workTimeoutS: 60 });
    await new Session(w, gw).run();

    expect(gw.sent[0]).toEqual({
      type: "config",
      config: {
        queues: ["in"],
        protocol_versions: [1],
        want_docs: false,
        want_success: false,
        want_dependency: false,
        work_timeout_s: 60,
        max_attempts: 3,
        max_claims: 0,
        retry_delay_s: 0,
        error_queue: "",
      },
    });
    expect(gw.sent.map((m) => m.type)).toEqual(["config", "ready", "modify"]);
    expect(gw.sent[2].modification).toEqual({ deletes: [{ queue: "in", id: "t1", version: 3 }] });
  });

  it("names doc sets in take and hands work their docs", async () => {
    const gw = oneTask({ docs: true, success: true });
    const seen: { docs: Doc[]; sets: Doc[]; value: any }[] = [];
    let succeeded = false;
    const w = new Worker(["in"], {
      take: () => [claimSet("ns", "k")],
      work: (w) => {
        seen.push({ docs: w.docs, sets: w.sets, value: w.value });
        return M.of(M.deleting(w.task), M.changing(w.docs[0], { content: { a: 2 } }));
      },
      onSuccess: () => {
        succeeded = true;
      },
    });
    await new Session(w, gw).run();

    expect(gw.sent[0].config.want_docs).toBe(true);
    expect(gw.sent[0].config.want_success).toBe(true);
    expect(gw.sent.find((m) => m.type === "take")!.doc_set_claims).toEqual([
      { set: { namespace: "ns", key: "k", version: 0 } },
    ]);
    expect(seen).toEqual([{ docs: [doc], sets: [{ ...doc, id: "", content: undefined }], value: { n: 1 } }]);
    // A doc change keeps the keys it was handed, the secondary one included.
    expect(gw.sent.find((m) => m.type === "modify")!.modification.docChanges).toEqual([
      {
        oldId: { namespace: "ns", id: "d1", version: 2 },
        newData: { namespace: "ns", id: "d1", key: "k", secondaryKey: "s", content: { a: 2 } },
      },
    ]);
    expect(succeeded).toBe(true);
    expect(gw.sent.at(-1)!.type).toBe("done");
  });

  it.each([
    ["retry", () => new RetryError("later", "dead"), { outcome: "retry", error: { message: "later", queue: "dead" } }],
    ["move", () => new MoveError("bad", "elsewhere"), { outcome: "move", error: { message: "bad", queue: "elsewhere" } }],
    ["fatal", () => new FatalWorker("broken"), { outcome: "fatal", error: { message: "broken" } }],
    ["error", () => new TypeError("oops"), { outcome: "error", error: { message: "TypeError: oops" } }],
  ])("answers a thrown %s with its outcome", async (_name, err, fate) => {
    const gw = oneTask();
    await new Session(new Worker(["in"], { work: () => { throw err(); } }), gw).run();
    const modify = gw.sent.find((m) => m.type === "modify")!;
    expect(modify).toEqual({ type: "modify", ...fate });
  });

  it("answers an idle ready with ready until a task comes", async () => {
    let idle = 2;
    const gw = new FakeGateway((msg) => {
      if (msg.type === "config") return { type: "ready", session: "s1" };
      if (msg.type === "ready") return idle-- > 0 ? { type: "ready" } : { type: "work", task, doc_sets: [] };
      return { type: "quit" };
    });
    await new Session(new Worker(["in"], { work: () => null }), gw).run();
    expect(gw.sent.map((m) => m.type)).toEqual(["config", "ready", "ready", "ready", "modify"]);
    expect(gw.sent.at(-1)!.modification).toBeUndefined();
  });

  it("says quit on every message once the worker stops itself", async () => {
    const gw = oneTask({ success: true });
    const w = new Worker(["in"], {
      work: (w, ctx) => {
        ctx.stop();
        expect(ctx.signal.aborted).toBe(true);
        return M.of(M.deleting(w.task));
      },
      onSuccess: () => undefined,
    });
    await new Session(w, gw).run();
    expect(gw.sent.slice(0, 2).every((m) => !m.quit)).toBe(true);
    expect(gw.sent.slice(2).every((m) => m.quit === true)).toBe(true);
  });

  it("keeps long work alive, and sends nothing after the answer", async () => {
    const gw = oneTask({ silenceS: 0.09 }); // a keepalive every 30ms
    const w = new Worker(["in"], {
      work: async (w, ctx) => {
        await ctx.sleep(100);
        return M.of(M.deleting(w.task));
      },
    });
    await new Session(w, gw).run();
    const types = gw.sent.map((m) => m.type);
    expect(types.filter((t) => t === "working").length).toBeGreaterThanOrEqual(2);
    expect(types.at(-1)).toBe("modify");
  });

  it("lets a read's error end the session, whatever the function wanted", async () => {
    const gw = new FakeGateway((msg) => {
      if (msg.type === "config") return { type: "ready", session: "s1" };
      if (msg.type === "ready") return { type: "work", task, doc_sets: [] };
      if (msg.type === "tasks") return { type: "error", class: "gone", message: "no such session" };
      return { type: "quit" };
    });
    const w = new Worker(["in"], {
      work: async (_w, ctx) => {
        await ctx.read("tasks", { tasks_query: { queue: "in" } });
        return null;
      },
    });
    await expect(new Session(w, gw).run()).rejects.toBeInstanceOf(SessionGone);
  });
});

describe("Modification", () => {
  it("delays an insert or change by byMs, as a duration", () => {
    const m = M.of(M.inserting({ queue: "next", value: 1, byMs: 1500 }), M.changing(task, { queue: "later", byMs: 20 }));
    expect(m.request.inserts).toEqual([{ queue: "next", value: 1, byMs: "1500" }]);
    expect(m.request.changes).toEqual([
      { oldId: { queue: "in", id: "t1", version: 3 }, newData: { queue: "later", value: { n: 1 }, byMs: "20" } },
    ]);
  });

  it("names docs and tasks by id and version", () => {
    const m = M.of(M.depending(doc), M.deleting(doc), M.inserting({ namespace: "ns", key: "k2", content: 3 }));
    expect(m.request).toEqual({
      docDepends: [{ namespace: "ns", id: "d1", version: 2 }],
      docDeletes: [{ namespace: "ns", id: "d1", version: 2 }],
      docInserts: [{ namespace: "ns", key: "k2", content: 3 }],
    });
  });
});

describe("Gateway", () => {
  it("starts a new session at once when one is gone", async () => {
    let sessions = 0;
    const gw = new Gateway({
      log: () => undefined,
      connect: async () => {
        sessions++;
        if (sessions === 1) {
          return new FakeGateway(() => ({ type: "error", class: "gone", message: "gone" }));
        }
        return oneTask();
      },
    });
    let worked = 0;
    const w = new Worker(["in"], {
      work: (w, ctx) => {
        worked++;
        ctx.stop();
        return M.of(M.deleting(w.task));
      },
    });
    await gw.run(w);
    expect(sessions).toBe(2);
    expect(worked).toBe(1);
  });

  it("throws a failure no new session can fix, and stops every worker", async () => {
    const gw = new Gateway({
      log: () => undefined,
      connect: async () => new FakeGateway(() => ({ type: "error", class: "caller", message: "bad config" })),
    });
    const w = new Worker(["in"], { work: () => null });
    await expect(gw.run(w, w)).rejects.toThrow("[caller] bad config");
  });

  it("drains on stop: the task in hand finishes, and the session says quit", async () => {
    let gate!: () => void;
    const holding = new Promise<void>((resolve) => (gate = resolve));
    const fake = oneTask();
    const gw = new Gateway({ log: () => undefined, connect: async () => fake });
    const w = new Worker(["in"], {
      work: async (w) => {
        gate();
        await new Promise((r) => setTimeout(r, 20));
        return M.of(M.deleting(w.task));
      },
    });
    const running = gw.run(w);
    await holding;
    gw.stop();
    await running;
    const modify = fake.sent.find((m) => m.type === "modify")!;
    expect(modify.modification).toEqual({ deletes: [{ queue: "in", id: "t1", version: 3 }] });
    expect(modify.quit).toBe(true);
  });
});

describe("HTTPTransport", () => {
  async function serve(handler: (body: any) => { status: number; type: string; body: string }) {
    const server: Server = createServer((req, res) => {
      let data = "";
      req.on("data", (c) => (data += c));
      req.on("end", () => {
        const out = handler(JSON.parse(data));
        res.writeHead(out.status, { "Content-Type": out.type });
        res.end(out.body);
      });
    });
    await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
    const { port } = server.address() as AddressInfo;
    return { url: `http://127.0.0.1:${port}/`, close: () => new Promise((r) => server.close(r)) };
  }

  it("carries the session the gateway named on every later message", async () => {
    const got: any[] = [];
    const srv = await serve((body) => {
      got.push(body);
      return { status: 200, type: "application/json", body: JSON.stringify({ type: "ready", session: "abc" }) };
    });
    try {
      const t = new HTTPTransport(srv.url);
      await t.exchange({ type: "config" });
      await t.exchange({ type: "ready" });
      expect(got.map((b) => b.session)).toEqual(["", "abc"]);
    } finally {
      await srv.close();
    }
  });

  it("raises the class of an error answer, and gone as SessionGone", async () => {
    const srv = await serve((body) =>
      body.type === "config"
        ? { status: 503, type: "application/json", body: JSON.stringify({ type: "error", class: "transient", message: "down" }) }
        : { status: 404, type: "application/json", body: JSON.stringify({ type: "error", class: "gone", message: "who" }) },
    );
    try {
      const t = new HTTPTransport(srv.url);
      await expect(t.exchange({ type: "config" })).rejects.toMatchObject({ cls: "transient" });
      await expect(t.exchange({ type: "ready" })).rejects.toBeInstanceOf(SessionGone);
    } finally {
      await srv.close();
    }
  });

  it("classes a body that is not the gateway's by its status", async () => {
    const srv = await serve(() => ({ status: 502, type: "text/html", body: "<h1>Bad Gateway</h1>" }));
    try {
      await expect(new HTTPTransport(srv.url).exchange({ type: "config" })).rejects.toMatchObject({ cls: "gateway" });
    } finally {
      await srv.close();
    }
  });
});
