"use strict";
Object.defineProperty(exports, "__esModule", { value: true });
exports.EntroQWorker = exports.EntroQMoveError = exports.EntroQRetryError = void 0;
/**
 * EntroQRetryError indicates that the task should be retried after a delay.
 */
class EntroQRetryError extends Error {
    constructor(message, delayMs) {
        super(message);
        this.delayMs = delayMs;
        this.name = "EntroQRetryError";
    }
}
exports.EntroQRetryError = EntroQRetryError;
/**
 * EntroQMoveError indicates that the task should be moved to a different queue.
 */
class EntroQMoveError extends Error {
    constructor(message, queue) {
        super(message);
        this.queue = queue;
        this.name = "EntroQMoveError";
    }
}
exports.EntroQMoveError = EntroQMoveError;
/**
 * EntroQWorker provides a high-level looping protocol for processing tasks.
 *
 * NOTE ON AUTO-RENEWAL:
 * This worker implementation currently OMITs automatic task renewal (heartbeating).
 * Task starvation is a real concern if a worker hangs or crashes while holding a
 * "forever-renewing" lease. Users should ensure their leaseMs is sufficient for the
 * handler's execution time, or manually renew tasks if needed.
 */
class EntroQWorker {
    constructor(client, options = {}) {
        this.running = false;
        this.client = client;
        this.options = {
            leaseMs: options.leaseMs || 30000,
            pollMs: options.pollMs || 5000,
            backoffMs: options.backoffMs || 10000,
        };
    }
    /**
     * run starts the worker loop across one or more queues.
     * The loop continues until stop() is called.
     */
    async run(queues, handler) {
        if (this.running) {
            throw new Error("Worker is already running");
        }
        this.running = true;
        while (this.running) {
            try {
                const resp = await this.client.claim(queues, this.options.leaseMs, this.options.pollMs);
                if (!resp.task) {
                    continue;
                }
                const task = resp.task;
                try {
                    const result = await handler(task);
                    await this.finalize(task, result);
                }
                catch (err) {
                    await this.handleTaskError(task, err);
                }
            }
            catch (err) {
                // Infrastructure error (network, server down, etc.)
                console.error("EntroQ Worker infrastructure error, backing off:", err);
                await new Promise((resolve) => setTimeout(resolve, this.options.backoffMs));
            }
        }
    }
    /**
     * stop signals the worker to stop processing new tasks after the current one finishes.
     */
    stop() {
        this.running = false;
    }
    async finalize(task, result) {
        if (result === "delete") {
            await this.client.modify({
                deletes: [this.toTaskID(task)],
            });
            return;
        }
        if (this.isModifyRequest(result)) {
            await this.client.modify(result);
            return;
        }
        // Default: If handler returns void, we assume it's done and should be deleted.
        // This matches the common use case.
        await this.client.modify({
            deletes: [this.toTaskID(task)],
        });
    }
    async handleTaskError(task, err) {
        if (err instanceof EntroQRetryError) {
            const atMs = Date.now() + (err.delayMs || 30000);
            await this.client.modify({
                changes: [
                    {
                        oldId: this.toTaskID(task),
                        newData: {
                            queue: task.queue,
                            atMs: atMs.toString(),
                            value: task.value,
                            attempt: (task.attempt || 0) + 1,
                            err: err.message,
                        },
                    },
                ],
            });
            return;
        }
        if (err instanceof EntroQMoveError) {
            await this.client.modify({
                changes: [
                    {
                        oldId: this.toTaskID(task),
                        newData: {
                            queue: err.queue,
                            atMs: "0",
                            value: task.value,
                            attempt: task.attempt,
                            err: err.message,
                        },
                    },
                ],
            });
            return;
        }
        // Unhandled error: just log it and let the lease expire 
        // or we could move it to an error queue automatically.
        // For now, we take the "fail safe" approach of letting it expire.
        console.error(`Task ${task.id} failed with unhandled error:`, err);
    }
    toTaskID(task) {
        return {
            id: task.id,
            version: task.version,
            queue: task.queue,
        };
    }
    isModifyRequest(res) {
        return res && typeof res === "object" && !Array.isArray(res);
    }
}
exports.EntroQWorker = EntroQWorker;
