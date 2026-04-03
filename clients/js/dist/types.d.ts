/**
 * TaskID contains the ID and version of a task.
 */
export interface TaskID {
    id: string;
    version: number;
    queue: string;
}
/**
 * TaskData contains only the data portion of a task.
 */
export interface TaskData {
    queue: string;
    atMs: string;
    value: string;
    id?: string;
    attempt?: number;
    err?: string;
}
/**
 * TaskChange identifies a task by ID and specifies new data.
 */
export interface TaskChange {
    oldId: TaskID;
    newData: TaskData;
}
/**
 * Task is a complete task object.
 */
export interface Task {
    queue: string;
    id: string;
    version: number;
    atMs: string;
    claimantId: string;
    value: string;
    createdMs: string;
    modifiedMs: string;
    claims: number;
    attempt: number;
    err: string;
}
/**
 * QueueStats contains statistics about a queue.
 */
export interface QueueStats {
    name: string;
    numTasks: number;
    numClaimed: number;
    numAvailable: number;
    maxClaims: number;
}
/**
 * ClaimRequest to attempt a claim on one or more queues.
 */
export interface ClaimRequest {
    claimantId: string;
    queues: string[];
    durationMs: string;
    pollMs: string;
}
/**
 * ClaimResponse from a claim attempt.
 */
export interface ClaimResponse {
    task?: Task;
}
/**
 * ModifyRequest to atomically update, insert, or delete tasks.
 */
export interface ModifyRequest {
    claimantId: string;
    inserts?: TaskData[];
    changes?: TaskChange[];
    deletes?: TaskID[];
    depends?: TaskID[];
}
/**
 * ModifyResponse with results of a transaction.
 */
export interface ModifyResponse {
    inserted?: Task[];
    changed?: Task[];
}
/**
 * TasksRequest to list tasks in a queue.
 */
export interface TasksRequest {
    claimantId?: string;
    queue: string;
    limit?: number;
    taskId?: string[];
    omitValues?: boolean;
}
/**
 * TasksResponse with listed tasks.
 */
export interface TasksResponse {
    tasks: Task[];
}
/**
 * QueuesRequest to list known queues.
 */
export interface QueuesRequest {
    matchPrefix?: string[];
    matchExact?: string[];
    limit?: number;
}
/**
 * QueuesResponse with listed queue stats.
 */
export interface QueuesResponse {
    queues: QueueStats[];
}
/**
 * TimeResponse with server's current time.
 */
export interface TimeResponse {
    timeMs: string;
}
