import { Task, Doc, ModifyRequest, TaskID, DocID, ClaimResponse, EntroQClientInterface } from "./types";

/**
 * EntroQStopWorker signals the worker to stop cleanly after the current task.
 */
export class EntroQStopWorker extends Error {
  constructor(message = "") {
    super(message);
    this.name = "EntroQStopWorker";
  }
}

/**
 * EntroQRetryError re-queues the task for retry after a delay.
 */
export class EntroQRetryError extends Error {
  constructor(message: string, public readonly delayMs?: number) {
    super(message);
    this.name = "EntroQRetryError";
  }
}

/**
 * EntroQMoveError moves the task to a different queue.
 * If queue is empty, the claim expires naturally (uses worker errQueue if set).
 */
export class EntroQMoveError extends Error {
  constructor(message: string, public readonly queue: string = "") {
    super(message);
    this.name = "EntroQMoveError";
  }
}

/**
 * WorkerDocClaim describes a set of docs to claim atomically before do_work.
 */
export interface WorkerDocClaim {
  namespace: string;
  key: string;
  /** Defaults to the worker's leaseMs. */
  durationMs?: number;
}

export interface WorkerOptions {
  /** How long to hold the task claim (and each renewal). Default: 30000ms. */
  leaseMs?: number;
  /** How long to poll when no task is available. Default: 5000ms. */
  pollMs?: number;
  /** Backoff after an infrastructure error. Default: 10000ms. */
  backoffMs?: number;
  /** Default error queue for EntroQMoveError with no queue. Default: "". */
  errQueue?: string;
  /** Default retry delay for EntroQRetryError with no delayMs. Default: 30000ms. */
  retryDelayMs?: number;
  /**
   * If > 0, tasks at or above this attempt count are moved to errQueue (or
   * queue + "/error") instead of being dispatched. Default: 0 (disabled).
   */
  maxAttempts?: number;
}

type DoWorkFn = (task: Task, docs: Doc[]) => Promise<Omit<ModifyRequest, "claimantId"> | void>;
type SelectorFn = (task: Task) => Promise<WorkerDocClaim[]>;
type FinisherFn = (task: Task, docs: Doc[]) => Promise<void>;

/**
 * HandlerBuilder assembles a handler from plain async functions.
 *
 * Chain .selector() and .finisher() to add optional phases:
 *
 *   const handle = EntroQWorker.handler(async (task, docs) => {
 *     return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
 *   }).selector(async (task) => [
 *     { namespace: "config", key: task.queue + "/settings" },
 *   ]).finisher(async (task, docs) => {
 *     // custom finalization when doWork returns void
 *   });
 *
 *   await worker.run(["my-queue"], handle);
 */
export class HandlerBuilder {
  constructor(
    readonly _doWork: DoWorkFn,
    readonly _selector?: SelectorFn,
    readonly _finisher?: FinisherFn,
  ) {}

  /** Return a new HandlerBuilder with the given selector registered. */
  selector(fn: SelectorFn): HandlerBuilder {
    return new HandlerBuilder(this._doWork, fn, this._finisher);
  }

  /** Return a new HandlerBuilder with the given finisher registered. */
  finisher(fn: FinisherFn): HandlerBuilder {
    return new HandlerBuilder(this._doWork, this._selector, fn);
  }
}

/**
 * EntroQWorker claims tasks from queues and dispatches them to a handler,
 * renewing claims (and any claimed docs) automatically in the background.
 *
 * Example:
 *
 *   const handle = EntroQWorker.handler(async (task, docs) => {
 *     await doWork(task.value);
 *     return { deletes: [{ id: task.id, version: task.version, queue: task.queue }] };
 *   });
 *
 *   const worker = new EntroQWorker(client, { leaseMs: 30000 });
 *   await worker.run(["my-queue"], handle);
 */
export class EntroQWorker {
  private readonly _opts: Required<WorkerOptions>;
  private _ac = new AbortController();

  constructor(
    private readonly _client: EntroQClientInterface,
    options: WorkerOptions = {},
  ) {
    this._opts = {
      leaseMs: options.leaseMs ?? 30000,
      pollMs: options.pollMs ?? 5000,
      backoffMs: options.backoffMs ?? 10000,
      errQueue: options.errQueue ?? "",
      retryDelayMs: options.retryDelayMs ?? 30000,
      maxAttempts: options.maxAttempts ?? 0,
    };
  }

  /** Build a HandlerBuilder from a doWork function. */
  static handler(fn: DoWorkFn): HandlerBuilder {
    return new HandlerBuilder(fn);
  }

  /**
   * Signal the worker to stop. Unblocks any waiting claim() immediately and
   * exits after the current task (if any) finishes.
   */
  stop(): void {
    this._ac.abort();
  }

  /**
   * Run the worker loop across the given queues until stop() is called,
   * the task throws EntroQStopWorker, or the loop is cancelled.
   *
   * Accepts a HandlerBuilder (from EntroQWorker.handler) or a plain doWork
   * function (no selector or finisher).
   */
  async run(queues: string | string[], handler: HandlerBuilder | DoWorkFn): Promise<void> {
    const qs = Array.isArray(queues) ? queues : [queues];
    this._ac = new AbortController();
    const { signal } = this._ac;
    const h = handler instanceof HandlerBuilder ? handler : new HandlerBuilder(handler);

    // Returns a promise that resolves (with null) when stop() is called.
    const stopRace = (): Promise<null> =>
      new Promise<null>(resolve => {
        if (signal.aborted) return resolve(null);
        signal.addEventListener("abort", () => resolve(null), { once: true });
      });

    while (!signal.aborted) {
      let claimResult: ClaimResponse | null;
      try {
        claimResult = await Promise.race([
          this._client.claim(qs, this._opts.leaseMs, this._opts.pollMs),
          stopRace(),
        ]);
      } catch (err) {
        console.error("Claim failed, retrying after backoff:", err);
        await sleep(this._opts.backoffMs);
        continue;
      }

      if (claimResult === null || signal.aborted) break;
      if (!claimResult.task) continue;

      try {
        if (!await this._process(claimResult.task, h)) break;
      } catch (err) {
        console.error("Worker error, retrying after backoff:", err);
        await sleep(this._opts.backoffMs);
      }
    }
  }

  private async _claimDocs(task: Task, h: HandlerBuilder): Promise<Doc[]> {
    if (!h._selector) return [];
    const claims = await h._selector(task);
    if (!claims.length) return [];

    // Sort by (namespace, key) to avoid deadlock when multiple workers race
    // for overlapping doc sets.
    const sorted = [...claims].sort((a, b) =>
      a.namespace !== b.namespace
        ? a.namespace < b.namespace ? -1 : 1
        : a.key < b.key ? -1 : a.key > b.key ? 1 : 0,
    );

    const docs: Doc[] = [];
    for (const claim of sorted) {
      const resp = await this._client.claimDocs({
        claimQuery: {
          namespace: claim.namespace,
          key: claim.key,
          durationMs: String(claim.durationMs ?? this._opts.leaseMs),
        },
      });
      docs.push(...resp.docs);
    }
    return docs;
  }

  private async _process(task: Task, h: HandlerBuilder): Promise<boolean> {
    if (this._opts.maxAttempts > 0 && task.attempt >= this._opts.maxAttempts) {
      const dest = this._opts.errQueue || `${task.queue}/error`;
      await this._client.modify({
        changes: [{
          oldId: toTaskID(task),
          newData: {
            queue: dest,
            atMs: "0",
            value: task.value,
            attempt: task.attempt,
            err: `max attempts (${this._opts.maxAttempts}) exceeded`,
          },
        }],
      });
      return true;
    }

    const docs = await this._claimDocs(task, h);

    let currentTask = task;
    let currentDocs = [...docs];
    let renewalErr: unknown = undefined;

    const ac = new AbortController();
    let renewHandle: ReturnType<typeof setTimeout> | null = null;

    const renew = async (): Promise<void> => {
      if (ac.signal.aborted) return;
      try {
        const atMs = String(Date.now() + this._opts.leaseMs);
        const docChanges = currentDocs.map(d => ({
          oldId: toDocID(d),
          newData: { namespace: d.namespace, id: d.id, key: d.key, secondaryKey: d.secondaryKey, content: d.content, atMs },
        }));
        const resp = await this._client.modify({
          changes: [{
            oldId: toTaskID(currentTask),
            newData: { queue: currentTask.queue, atMs, value: currentTask.value, attempt: currentTask.attempt, err: currentTask.err || undefined },
          }],
          docChanges: docChanges.length ? docChanges : undefined,
        });
        if (resp.changed?.length) currentTask = resp.changed[0];
        if (resp.changedDocs?.length) {
          const updated = new Map(resp.changedDocs.map(d => [`${d.namespace}/${d.id}`, d]));
          currentDocs = currentDocs.map(d => updated.get(`${d.namespace}/${d.id}`) ?? d);
        }
        scheduleRenew();
      } catch (err) {
        renewalErr = err;
        ac.abort();
      }
    };

    const scheduleRenew = (): void => {
      if (ac.signal.aborted) return;
      renewHandle = setTimeout(() => void renew(), this._opts.leaseMs / 2);
    };

    scheduleRenew();

    let result: Omit<ModifyRequest, "claimantId"> | void = undefined;
    let handlerErr: unknown = undefined;

    try {
      result = await h._doWork(task, docs);
    } catch (err) {
      handlerErr = err;
    } finally {
      ac.abort();
      if (renewHandle !== null) {
        clearTimeout(renewHandle);
        renewHandle = null;
      }
    }

    if (renewalErr !== undefined) throw renewalErr as Error;

    if (handlerErr instanceof EntroQStopWorker) return false;

    if (handlerErr instanceof EntroQRetryError) {
      const delay = handlerErr.delayMs ?? this._opts.retryDelayMs;
      const atMs = String(Date.now() + delay);
      const docChanges = currentDocs.map(d => ({
        oldId: toDocID(d),
        newData: { namespace: d.namespace, id: d.id, key: d.key, secondaryKey: d.secondaryKey, content: d.content, atMs: "0" },
      }));
      await this._client.modify({
        changes: [{
          oldId: toTaskID(currentTask),
          newData: { queue: currentTask.queue, atMs, value: currentTask.value, attempt: currentTask.attempt + 1, err: handlerErr.message },
        }],
        docChanges: docChanges.length ? docChanges : undefined,
      });
      return true;
    }

    if (handlerErr instanceof EntroQMoveError) {
      const dest = handlerErr.queue || this._opts.errQueue;
      if (dest) {
        const docChanges = currentDocs.map(d => ({
          oldId: toDocID(d),
          newData: { namespace: d.namespace, id: d.id, key: d.key, secondaryKey: d.secondaryKey, content: d.content, atMs: "0" },
        }));
        await this._client.modify({
          changes: [{
            oldId: toTaskID(currentTask),
            newData: { queue: dest, atMs: "0", value: currentTask.value, attempt: currentTask.attempt, err: handlerErr.message },
          }],
          docChanges: docChanges.length ? docChanges : undefined,
        });
      }
      // No dest: both task and doc claims expire naturally.
      return true;
    }

    if (handlerErr !== undefined) {
      console.error(`Task ${task.id} failed with unhandled error:`, handlerErr);
      return true;
    }

    if (result !== undefined) {
      await this._client.modify(fixVersions(result, currentTask, currentDocs));
    }
    await h._finisher?.(currentTask, currentDocs);

    return true;
  }
}

function toTaskID(task: Task): TaskID {
  return { id: task.id, version: task.version, queue: task.queue };
}

function toDocID(doc: Doc): DocID {
  return { namespace: doc.namespace, id: doc.id, version: doc.version };
}

/**
 * Return a copy of req with task and doc versions updated to the latest
 * versions from the renewal state. Called after renewal stops.
 */
function fixVersions(
  req: Omit<ModifyRequest, "claimantId">,
  task: Task,
  docs: Doc[],
): Omit<ModifyRequest, "claimantId"> {
  const taskVer = task.version;
  const docVers = new Map(docs.map(d => [`${d.namespace}/${d.id}`, d.version]));

  const fixT = (t: TaskID): TaskID =>
    t.id === task.id ? { ...t, version: taskVer } : t;
  const fixD = (d: DocID): DocID => {
    const v = docVers.get(`${d.namespace}/${d.id}`);
    return v !== undefined ? { ...d, version: v } : d;
  };

  return {
    ...req,
    changes: req.changes?.map(c => ({ ...c, oldId: fixT(c.oldId) })),
    deletes: req.deletes?.map(fixT),
    depends: req.depends?.map(fixT),
    docChanges: req.docChanges?.map(c => ({ ...c, oldId: fixD(c.oldId) })),
    docDeletes: req.docDeletes?.map(fixD),
    docDepends: req.docDepends?.map(fixD),
  };
}

function sleep(ms: number): Promise<void> {
  return new Promise(resolve => setTimeout(resolve, ms));
}
