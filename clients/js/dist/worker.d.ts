import { EntroQClient } from "./client";
import { Task, ModifyRequest } from "./types";
/**
 * EntroQRetryError indicates that the task should be retried after a delay.
 */
export declare class EntroQRetryError extends Error {
    delayMs?: number | undefined;
    constructor(message: string, delayMs?: number | undefined);
}
/**
 * EntroQMoveError indicates that the task should be moved to a different queue.
 */
export declare class EntroQMoveError extends Error {
    queue: string;
    constructor(message: string, queue: string);
}
export interface WorkerOptions {
    /**
     * How long to claim the task for initially (in milliseconds).
     * Default is 30,000 (30 seconds).
     */
    leaseMs?: number;
    /**
     * How long to wait between polls (in milliseconds).
     * Default is 5,000 (5 seconds).
     */
    pollMs?: number;
    /**
     * How long to wait after a network or infrastructure error before retrying (in milliseconds).
     * Default is 10,000 (10 seconds).
     */
    backoffMs?: number;
}
export type WorkHandler = (task: Task) => Promise<void | ModifyRequest | "delete">;
/**
 * EntroQWorker provides a high-level looping protocol for processing tasks.
 *
 * NOTE ON AUTO-RENEWAL:
 * This worker implementation currently OMITs automatic task renewal (heartbeating).
 * Task starvation is a real concern if a worker hangs or crashes while holding a
 * "forever-renewing" lease. Users should ensure their leaseMs is sufficient for the
 * handler's execution time, or manually renew tasks if needed.
 */
export declare class EntroQWorker {
    private client;
    private options;
    private running;
    constructor(client: EntroQClient, options?: WorkerOptions);
    /**
     * run starts the worker loop across one or more queues.
     * The loop continues until stop() is called.
     */
    run(queues: string[], handler: WorkHandler): Promise<void>;
    /**
     * stop signals the worker to stop processing new tasks after the current one finishes.
     */
    stop(): void;
    private finalize;
    private handleTaskError;
    private toTaskID;
    private isModifyRequest;
}
