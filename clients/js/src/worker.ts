import { EntroQClient } from "./client";
import { Task, ModifyRequest, TaskID } from "./types";

/**
 * EntroQRetryError indicates that the task should be retried after a delay.
 */
export class EntroQRetryError extends Error {
  constructor(message: string, public delayMs?: number) {
    super(message);
    this.name = "EntroQRetryError";
  }
}

/**
 * EntroQMoveError indicates that the task should be moved to a different queue.
 */
export class EntroQMoveError extends Error {
  constructor(message: string, public queue: string) {
    super(message);
    this.name = "EntroQMoveError";
  }
}

export interface WorkerOptions {
  /**
   * How long to claim the task for initially, and each renewal (in milliseconds).
   * Default is 30,000 (30 seconds).
   */
  leaseMs?: number;
  /**
   * How long to wait between polls when no task is available (in milliseconds).
   * Default is 5,000 (5 seconds).
   */
  pollMs?: number;
  /**
   * How long to wait after an infrastructure error before retrying (in milliseconds).
   * Default is 10,000 (10 seconds).
   */
  backoffMs?: number;
}

/**
 * WorkHandler receives the claimed task and a stop function.
 *
 * The worker renews the task's claim in the background automatically.
 * When heavy work is done, call stop() to halt renewal and get back
 * the stable task version -- the one to use for any modify (delete,
 * change, depend, etc.).
 *
 * Example:
 *
 *   worker.run(['my-queue'], async (task, stop) => {
 *     const result = await doHeavyWork(task.value);
 *     const stable = await stop();
 *     return { deletes: [toTaskID(stable)] };
 *   });
 */
export type WorkHandler = (
  task: Task,
  stop: () => Promise<Task>
) => Promise<Omit<ModifyRequest, "claimantId"> | "delete" | void>;

/**
 * EntroQWorker claims tasks from one or more queues and dispatches them
 * to a handler, with automatic claim renewal in the background.
 */
export class EntroQWorker {
  private client: EntroQClient;
  private options: Required<WorkerOptions>;
  private running: boolean = false;

  constructor(client: EntroQClient, options: WorkerOptions = {}) {
    this.client = client;
    this.options = {
      leaseMs: options.leaseMs ?? 30000,
      pollMs: options.pollMs ?? 5000,
      backoffMs: options.backoffMs ?? 10000,
    };
  }

  /**
   * run starts the worker loop across one or more queues.
   * The loop continues until stop() is called.
   */
  async run(queues: string[], handler: WorkHandler): Promise<void> {
    if (this.running) {
      throw new Error("Worker is already running");
    }
    this.running = true;

    while (this.running) {
      try {
        const resp = await this.client.claim(
          queues,
          this.options.leaseMs,
          this.options.pollMs
        );

        if (!resp.task) {
          continue;
        }

        await this.dispatch(resp.task, handler);
      } catch (err) {
        console.error("EntroQ worker infrastructure error, backing off:", err);
        await sleep(this.options.backoffMs);
      }
    }
  }

  /**
   * stop signals the worker to stop after the current task finishes.
   */
  stop(): void {
    this.running = false;
  }

  private async dispatch(task: Task, handler: WorkHandler): Promise<void> {
    const leaseMs = this.options.leaseMs;

    // current holds the most recently renewed version of the task.
    let current = task;
    // renewalPromise tracks any in-flight renewal so stop() can wait for it.
    let renewalPromise: Promise<void> = Promise.resolve();

    const ac = new AbortController();
    let renewTimeout: ReturnType<typeof setTimeout> | null = null;
    let stopped = false;

    const scheduleRenew = (): void => {
      if (ac.signal.aborted) {
        return;
      }
      renewTimeout = setTimeout(() => {
        if (ac.signal.aborted) {
          return;
        }
        renewalPromise = (async () => {
          try {
            const atMs = (Date.now() + leaseMs).toString();
            const resp = await this.client.modify({
              changes: [{
                oldId: toTaskID(current),
                newData: {
                  queue: current.queue,
                  atMs,
                  value: current.value,
                  attempt: current.attempt,
                  err: current.err || undefined,
                },
              }],
            });
            if (resp.changed && resp.changed.length > 0) {
              current = resp.changed[0];
            }
            scheduleRenew();
          } catch (e) {
            // Renewal failed -- abort so stop() knows not to wait.
            ac.abort();
          }
        })();
      }, leaseMs / 2);
    };

    // stop halts the renewal loop and waits for any in-flight renewal to
    // settle before returning the stable task version.
    const stop = async (): Promise<Task> => {
      if (!stopped) {
        stopped = true;
        if (renewTimeout !== null) {
          clearTimeout(renewTimeout);
          renewTimeout = null;
        }
        ac.abort();
        await renewalPromise;
      }
      return current;
    };

    scheduleRenew();

    let result: Omit<ModifyRequest, "claimantId"> | "delete" | void = undefined;
    let handlerErr: unknown;

    try {
      result = await handler(task, stop);
    } catch (err) {
      handlerErr = err;
    } finally {
      // Always stop renewal, whether handler succeeded or threw.
      await stop();
    }

    if (handlerErr !== undefined) {
      await this.handleTaskError(current, handlerErr);
      return;
    }

    await this.finalize(current, result);
  }

  private async finalize(
    task: Task,
    result: Omit<ModifyRequest, "claimantId"> | "delete" | void
  ): Promise<void> {
    if (result === "delete" || result === undefined) {
      await this.client.modify({ deletes: [toTaskID(task)] });
      return;
    }
    await this.client.modify(result);
  }

  private async handleTaskError(task: Task, err: unknown): Promise<void> {
    if (err instanceof EntroQRetryError) {
      const atMs = (Date.now() + (err.delayMs ?? 30000)).toString();
      await this.client.modify({
        changes: [{
          oldId: toTaskID(task),
          newData: {
            queue: task.queue,
            atMs,
            value: task.value,
            attempt: (task.attempt ?? 0) + 1,
            err: err.message,
          },
        }],
      });
      return;
    }

    if (err instanceof EntroQMoveError) {
      await this.client.modify({
        changes: [{
          oldId: toTaskID(task),
          newData: {
            queue: err.queue,
            atMs: "0",
            value: task.value,
            attempt: task.attempt,
            err: err.message,
          },
        }],
      });
      return;
    }

    // Unhandled error: log and let the lease expire naturally.
    console.error(`Task ${task.id} failed with unhandled error:`, err);
  }
}

function toTaskID(task: Task): TaskID {
  return { id: task.id, version: task.version, queue: task.queue };
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
