/**
 * An EntroQ work gateway client: workers that run through `eqlink work`.
 *
 * The gateway runs the EntroQ worker loop in Go -- claim, renew, version
 * fix-up, retry, move, doc set claims -- and a worker here answers its small
 * JSON protocol (docs/workgateway-protocol.md). A worker supplies functions and
 * nothing else:
 *
 *     const w = new Worker(["in"], {
 *       work: async (w) => M.of(M.deleting(w.task)),
 *     });
 *     await new Gateway({ url }).run(w);
 *
 * `work` settles one task: what it returns is committed, and throwing
 * RetryError, MoveError or FatalWorker gives the task another fate. `take`
 * names the doc sets a task needs, `onSuccess` runs after a commit lands, and
 * `onDependency` picks a fate when a commit loses a dependency race.
 *
 * This module uses only fetch, AbortSignal and timers, so it runs wherever
 * those do, given the URL of a gateway already serving HTTP. `entroq/node`
 * starts one as a child process instead.
 */
import type { Doc, DocID, DocSetClaim, ModifyRequest, Task, TaskID } from "./types";

/** The gateway protocols this client speaks. */
export const PROTOCOLS = [1];

/** A protocol message, in either direction. */
export type Message = { type: string; [field: string]: any };

/**
 * The gateway ended the session. `cls` says whether a new one could help:
 * "transient" is worth retrying; "caller" and "gateway" are not.
 */
export class GatewayError extends Error {
  constructor(
    public readonly cls: string,
    message: string,
  ) {
    super(`[${cls}] ${message}`);
    this.name = "GatewayError";
  }
}

/** The gateway no longer has this session, so start a new one. */
export class SessionGone extends Error {
  constructor(message = "") {
    super(message);
    this.name = "SessionGone";
  }
}

/**
 * Throw to try the task again later, counting the attempt. Once the attempts
 * run out it moves to `orMoveTo`, or to the worker's error queue.
 */
export class RetryError extends Error {
  constructor(
    message = "",
    public readonly orMoveTo = "",
  ) {
    super(message);
    this.name = "RetryError";
  }
}

/** Throw to move the task now, to `queue` or to the worker's error queue. */
export class MoveError extends Error {
  constructor(
    message = "",
    public readonly queue = "",
  ) {
    super(message);
    this.name = "MoveError";
  }
}

/**
 * Throw to stop every worker with this error, for something no retry can fix
 * in this process. The task comes back when its lease lapses.
 */
export class FatalWorker extends Error {
  constructor(message = "") {
    super(message);
    this.name = "FatalWorker";
  }
}

/** The error an error message stands for: "gone" asks for a new session. */
export function failure(msg: Message): Error {
  if (msg.class === "gone") {
    return new SessionGone(msg.message ?? "");
  }
  return new GatewayError(msg.class ?? "gateway", msg.message ?? "");
}

/**
 * A task in hand: the task, its value, the docs claimed for it, and the sets
 * they were claimed as. `docs` and `sets` are empty in `take`, which decides
 * them.
 */
export class Work {
  readonly value: any;
  readonly sets: Doc[];
  readonly docs: Doc[];

  constructor(
    readonly task: Task,
    docSets: { set: Doc; docs?: Doc[] }[] = [],
  ) {
    this.value = task.value;
    this.sets = docSets.map((s) => s.set);
    this.docs = docSets.flatMap((s) => s.docs ?? []);
  }
}

/** A doc set for `take` to claim. */
export function claimSet(namespace: string, key: string, omitMembers = false): DocSetClaim {
  return omitMembers ? { set: { namespace, key, version: 0 }, omitMembers } : { set: { namespace, key, version: 0 } };
}

type Field = keyof Omit<ModifyRequest, "claimantId">;
type Op = [Field, any];

function isDoc(item: Task | Doc): item is Doc {
  return "namespace" in item;
}

function taskId(t: Task | TaskID): TaskID {
  return { queue: t.queue, id: t.id, version: t.version ?? 0 };
}

function docId(d: Doc | DocID): DocID {
  return { namespace: d.namespace, id: d.id, version: d.version ?? 0 };
}

function arriving<T extends object>(data: T, byMs?: number): T {
  return byMs ? { ...data, byMs: String(Math.round(byMs)) } : data;
}

/**
 * Task and doc operations committed together, or not at all:
 *
 *     M.of(M.deleting(w.task), M.inserting({ queue: "next", value: v }))
 *
 * Name a task or doc as it was handed over. The gateway moves the versions of
 * the task in hand and its claimed docs to wherever renewal left them.
 */
export class Modification {
  readonly request: Omit<ModifyRequest, "claimantId"> = {};

  constructor(...ops: Op[]) {
    for (const [field, item] of ops) {
      ((this.request[field] ??= []) as any[]).push(item);
    }
  }

  static of(...ops: Op[]): Modification {
    return new Modification(...ops);
  }

  /**
   * A task, given queue and value, and byMs to delay it; or a doc, given
   * namespace and key, and optionally secondaryKey and content.
   */
  static inserting(
    item:
      | { queue: string; value?: any; byMs?: number }
      | { namespace: string; key: string; secondaryKey?: string; content?: any; id?: string },
  ): Op {
    if ("queue" in item) {
      const { byMs, ...data } = item;
      return ["inserts", arriving(data, byMs)];
    }
    return ["docInserts", item];
  }

  /**
   * A task or doc with the fields named replaced: a task's queue or value,
   * which byMs may delay; a doc's content or keys.
   */
  static changing(item: Task | Doc, fields: { byMs?: number; [field: string]: any } = {}): Op {
    const { byMs, ...rest } = fields;
    if (isDoc(item)) {
      const { namespace, id, key, secondaryKey, content } = item;
      return ["docChanges", { oldId: docId(item), newData: { namespace, id, key, secondaryKey, content, ...rest } }];
    }
    const data = { queue: item.queue, value: item.value, ...rest };
    return ["changes", { oldId: taskId(item), newData: arriving(data, byMs) }];
  }

  static deleting(item: Task | Doc): Op {
    return isDoc(item) ? ["docDeletes", docId(item)] : ["deletes", taskId(item)];
  }

  /** Commit only if item is still at its version, changing nothing. */
  static depending(item: Task | Doc): Op {
    return isDoc(item) ? ["docDepends", docId(item)] : ["depends", taskId(item)];
  }
}

export const M = Modification;

/** One session's exchange: send a message, get the gateway's answer. */
export interface Transport {
  exchange(msg: Message): Promise<Message>;
  close(): void;
}

/**
 * One session over HTTP to a gateway at url: a POST per message. Closing it
 * hangs up any request in flight, which the gateway takes as the client
 * leaving.
 */
export class HTTPTransport implements Transport {
  private session = "";
  private readonly hangUp = new AbortController();

  constructor(
    private readonly url: string,
    private readonly fetcher: typeof fetch = fetch,
  ) {}

  async exchange(msg: Message): Promise<Message> {
    const resp = await this.fetcher(this.url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...msg, session: this.session }),
      signal: this.hangUp.signal,
    });
    const body = await resp.text();
    if (!(resp.headers.get("Content-Type") ?? "").startsWith("application/json")) {
      // Not the gateway's answer (a proxy's, say), so the status is all there
      // is to go on.
      throw new GatewayError(resp.status < 500 ? "caller" : "gateway", `HTTP ${resp.status}: ${body.trim()}`);
    }
    const reply = JSON.parse(body) as Message;
    if (reply.type === "error") {
      throw failure(reply);
    }
    if (!this.session) {
      this.session = reply.session ?? "";
    }
    return reply;
  }

  close(): void {
    this.hangUp.abort();
  }
}

/**
 * What a worker function gets besides its work: a signal that the worker is
 * stopping, a way to stop it, and reads that answer mid-turn.
 */
export class Context {
  constructor(
    private readonly send: (msg: Message) => Promise<Message>,
    private readonly stopping: AbortController,
  ) {}

  /** Aborted once the worker starts stopping. */
  get signal(): AbortSignal {
    return this.stopping.signal;
  }

  /**
   * Stop this worker once the task in hand is settled. Its fate still lands,
   * and its onSuccess or onDependency still runs.
   */
  stop(): void {
    this.stopping.abort();
  }

  /** Sleep, waking early if the worker starts stopping. True if it did not. */
  sleep(ms: number): Promise<boolean> {
    return sleep(ms, this.signal);
  }

  /**
   * Send a read ("time", "tasks", "docs", "queues", "namespaces") and return
   * its answer. A read is not a turn: the task is still in hand.
   */
  read(kind: string, fields: Record<string, any> = {}): Promise<Message> {
    return this.send({ ...fields, type: kind });
  }

  /** The backend's clock, in milliseconds since the epoch. */
  async time(): Promise<number> {
    return Number((await this.read("time")).time_ms);
  }
}

/** Resolve after ms, or early, with false, once signal aborts. */
export function sleep(ms: number, signal?: AbortSignal): Promise<boolean> {
  return new Promise((resolve) => {
    if (signal?.aborted) {
      resolve(false);
      return;
    }
    const done = (slept: boolean) => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      resolve(slept);
    };
    const onAbort = () => done(false);
    const timer = setTimeout(() => done(true), ms);
    signal?.addEventListener("abort", onAbort);
  });
}

/** The functions a worker runs. Only work is required. */
export interface Handlers {
  work(w: Work, ctx: Context): Promise<Modification | null | void> | Modification | null | void;
  take?(w: Work, ctx: Context): Promise<DocSetClaim[] | null | void> | DocSetClaim[] | null | void;
  onSuccess?(w: Work, ctx: Context): Promise<void> | void;
  onDependency?(w: Work, deps: any[], ctx: Context): Promise<void> | void;
}

/**
 * How a worker registers. Zero takes the gateway's default for each, and the
 * durations are whole seconds.
 */
export interface WorkerOptions {
  /** How long one task is expected to take; the operator caps it. */
  workTimeoutS?: number;
  /** Retries before a task is quarantined; zero is no limit. */
  maxAttempts?: number;
  /** Claims a task may fail to survive; zero is the default of five, negative is no limit. */
  maxClaims?: number;
  /**
   * Base delay before a retried task is available again, and before a task
   * whose doc sets were held is tried again. The worker's default is 30s.
   */
  retryDelayS?: number;
  /** Where a moved task goes; `{inbox}` is the task's own queue. */
  errorQueue?: string;
}

/** A worker: its functions and how it registers. It runs as one session. */
export class Worker {
  constructor(
    readonly queues: string[],
    readonly handlers: Handlers,
    readonly options: WorkerOptions = {},
  ) {}

  config(): Record<string, any> {
    const o = this.options;
    return {
      queues: this.queues,
      protocol_versions: PROTOCOLS,
      want_docs: this.handlers.take !== undefined,
      want_success: this.handlers.onSuccess !== undefined,
      want_dependency: this.handlers.onDependency !== undefined,
      work_timeout_s: o.workTimeoutS ?? 0,
      max_attempts: o.maxAttempts ?? 0,
      max_claims: o.maxClaims ?? 0,
      retry_delay_s: o.retryDelayS ?? 0,
      error_queue: o.errorQueue ?? "",
    };
  }
}

type Fate = { outcome?: string; error?: { message: string; queue?: string } };

/** One session: the protocol loop and its switch. */
export class Session {
  readonly stopping = new AbortController();
  private readonly ctx: Context;
  private keepaliveMs = 0;
  // One request at a time: a keepalive must not overlap the answer it keeps
  // alive, nor a read.
  private inFlight: Promise<unknown> = Promise.resolve();

  constructor(
    private readonly worker: Worker,
    private readonly xchg: Transport,
  ) {
    this.ctx = new Context((msg) => this.send(msg), this.stopping);
  }

  async run(): Promise<void> {
    let reply = await this.send({ type: "config", config: this.worker.config() });
    if (reply.type === "ready") {
      // Every third of the window, so one late keepalive is not fatal.
      this.keepaliveMs = ((reply.silence_timeout_s ?? 0) * 1000) / 3;
    }
    const h = this.worker.handlers;
    let work: Work | undefined;
    for (;;) {
      let msg: Message;
      switch (reply.type) {
        case "quit":
          return;
        case "ready":
          // The gateway is waiting on a claim, and says so now and then.
          msg = { type: "ready" };
          break;
        case "take": {
          work = new Work(reply.task);
          const w = work;
          const [claims, fate] = await this.settle(h.take && (() => h.take!(w, this.ctx)));
          msg = { type: "take", doc_set_claims: claims ?? [], ...fate };
          break;
        }
        case "work": {
          work = new Work(reply.task, reply.doc_sets ?? []);
          const w = work;
          const [mod, fate] = await this.settle(() => h.work(w, this.ctx));
          msg = { type: "modify", ...fate };
          if (mod) {
            msg.modification = mod.request;
          }
          break;
        }
        case "success": {
          const w = work!;
          const [, fate] = await this.settle(h.onSuccess && (() => h.onSuccess!(w, this.ctx)));
          msg = { type: "done", ...fate };
          break;
        }
        case "dependency": {
          const w = work!;
          const deps = reply.deps ?? [];
          const [, fate] = await this.settle(h.onDependency && (() => h.onDependency!(w, deps, this.ctx)));
          msg = { type: "done", ...fate };
          break;
        }
        default:
          throw new GatewayError("caller", `unexpected ${JSON.stringify(reply.type)}`);
      }
      reply = await this.send(msg);
    }
  }

  /** The loop's exchange, one at a time. Once stopping, every message carries quit. */
  send(msg: Message): Promise<Message> {
    const out = this.stopping.signal.aborted ? { ...msg, quit: true } : msg;
    const next = this.inFlight.then(() => this.xchg.exchange(out));
    this.inFlight = next.catch(() => undefined);
    return next;
  }

  /** Run fn, returning what it returns and, if it threw, the fate that asks for. */
  private async settle<T>(fn: (() => Promise<T> | T) | undefined): Promise<[T | undefined, Fate]> {
    if (!fn) {
      return [undefined, {}];
    }
    try {
      return [await this.keepingAlive(fn), {}];
    } catch (e) {
      if (e instanceof GatewayError || e instanceof SessionGone) {
        throw e; // a read that found the session ending; the task has no say
      }
      if (e instanceof RetryError) {
        return [undefined, { outcome: "retry", error: { message: e.message, queue: e.orMoveTo } }];
      }
      if (e instanceof MoveError) {
        return [undefined, { outcome: "move", error: { message: e.message, queue: e.queue } }];
      }
      if (e instanceof FatalWorker) {
        return [undefined, { outcome: "fatal", error: { message: e.message } }];
      }
      // A failure the function did not classify stops the worker, after the
      // gateway quarantines the task if this was its last claim.
      const message = e instanceof Error ? `${e.name}: ${e.message}` : String(e);
      return [undefined, { outcome: "error", error: { message } }];
    }
  }

  /**
   * Run fn, telling the gateway the client is still working if it runs long.
   * Short work sends nothing; work that outlasts one interval keeps sending
   * until it returns.
   */
  private async keepingAlive<T>(fn: () => Promise<T> | T): Promise<T> {
    if (!this.keepaliveMs) {
      return fn();
    }
    const timer = setInterval(() => {
      // A failure means the session is ending; the answer will say so.
      this.ctx.read("working").catch(() => undefined);
    }, this.keepaliveMs);
    try {
      return await fn();
    } finally {
      // Cleared before the answer is sent, so no keepalive follows it onto
      // the next task; one already queued goes first.
      clearInterval(timer);
    }
  }
}

/** How a Gateway reaches the gateway: a URL, or a way to open a session. */
export interface GatewayOptions {
  /** The URL of a gateway serving HTTP (`eqlink work --addr`). */
  url?: string;
  /** Opens one session's transport; overrides url. */
  connect?: () => Promise<Transport>;
  /** Where the gateway's notes go; console.error by default. */
  log?: (line: string) => void;
}

/**
 * Runs workers through a gateway, each as a session of its own, and stops
 * them together.
 */
export class Gateway {
  protected readonly stopping = new AbortController();
  private readonly sessions = new Set<Session>();
  private failure?: Error;
  private readonly connectFn: () => Promise<Transport>;
  protected readonly log: (line: string) => void;

  constructor(opts: GatewayOptions = {}) {
    const { url, connect } = opts;
    if (!connect && !url) {
      throw new Error("Gateway needs a url or a connect function");
    }
    this.connectFn = connect ?? (async () => new HTTPTransport(url!));
    this.log = opts.log ?? ((line) => console.error(line));
  }

  /** Open one session's transport. */
  protected connect(): Promise<Transport> {
    return this.connectFn();
  }

  /**
   * Run until every worker has stopped: by ctx.stop, by stop(), or by a
   * failure a new session cannot fix, which is then thrown.
   */
  async run(...workers: Worker[]): Promise<void> {
    await Promise.all(workers.map((w) => this.serve(w)));
    if (this.failure) {
      throw this.failure;
    }
  }

  /**
   * Drain: every task in hand finishes, and no session claims again. Each
   * session says quit, and the gateway answers it with quit.
   */
  stop(): void {
    this.stopping.abort();
    for (const s of this.sessions) {
      s.stopping.abort();
    }
  }

  /** Run one worker's sessions one after another until it stops. */
  private async serve(worker: Worker): Promise<void> {
    let backoffMs = 200;
    const wait = async () => {
      await sleep(backoffMs, this.stopping.signal);
      backoffMs = Math.min(backoffMs * 2, 5000);
    };
    while (!this.stopping.signal.aborted) {
      let xchg: Transport;
      try {
        xchg = await this.connect();
      } catch (e) {
        if (!this.recoverable(e)) {
          return;
        }
        await wait();
        continue;
      }
      const session = new Session(worker, xchg);
      this.sessions.add(session);
      if (this.stopping.signal.aborted) {
        session.stopping.abort();
      }
      try {
        await session.run();
        backoffMs = 200;
      } catch (e) {
        if (e instanceof SessionGone) {
          // Start a new session at once.
        } else if (e instanceof GatewayError) {
          if (!this.recoverable(e)) {
            return;
          }
          if (!session.stopping.signal.aborted) {
            await wait();
          }
        } else {
          // A gateway that went away, or never answered: try again, in time.
          this.log(`[worker] gateway unreachable (${e instanceof Error ? e.message : e}); reconnecting`);
          await wait();
        }
      } finally {
        this.sessions.delete(session);
        xchg.close();
      }
      if (session.stopping.signal.aborted) {
        return;
      }
    }
  }

  private recoverable(e: unknown): boolean {
    if (e instanceof GatewayError && e.cls === "transient") {
      this.log(`[worker] ${e.message}; reconnecting`);
      return true;
    }
    this.log(`[worker] ${e instanceof Error ? e.message : e}; stopping`);
    this.failure ??= e instanceof Error ? e : new Error(String(e));
    this.stop();
    return false;
  }
}
