import { Task, ClaimResponse, ModifyRequest, ModifyResponse, TasksRequest, TasksResponse, QueuesRequest, QueuesResponse } from "./types";
export interface ClientOptions {
    baseUrl: string;
    claimantId?: string;
    headers?: Record<string, string>;
}
export declare class EntroQClient {
    private baseUrl;
    private claimantId;
    private headers;
    constructor(options: ClientOptions);
    private generateClaimantId;
    private request;
    /**
     * tryClaim attempts to claim a task from one of the specified queues immediately.
     */
    tryClaim(queues: string[], durationMs?: number): Promise<ClaimResponse>;
    /**
     * claim attempts to claim a task, blocking or polling as necessary.
     */
    claim(queues: string[], durationMs?: number, pollMs?: number): Promise<ClaimResponse>;
    /**
     * modify atomically updates, inserts, deletes, or depends on tasks.
     */
    modify(request: Omit<ModifyRequest, "claimantId">): Promise<ModifyResponse>;
    /**
     * tasks lists tasks in a particular queue.
     */
    tasks(request: Omit<TasksRequest, "claimantId">): Promise<TasksResponse>;
    /**
     * queues lists statistics for multiple queues.
     */
    queues(request?: QueuesRequest): Promise<QueuesResponse>;
    /**
     * queueStats is a shortcut to get statistics specifically for the stats endpoint.
     */
    queueStats(request?: QueuesRequest): Promise<QueuesResponse>;
    /**
     * time returns the current server time in milliseconds since the epoch.
     */
    time(): Promise<string>;
    /**
     * streamTasks returns an async iterator that yields tasks as they are received from the server.
     * This uses HTTP Chunked Transfer Encoding to provide real-time updates.
     */
    streamTasks(request: Omit<TasksRequest, "claimantId">): AsyncIterable<Task>;
    /**
     * getClaimantId returns the current claimant ID being used by this client.
     */
    getClaimantId(): string;
}
