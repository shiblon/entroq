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
  atMs: string; // int64 -> string
  value: any; // bytes -> JSON value
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
  atMs: string; // int64 -> string
  claimantId: string;
  value: any; // bytes -> JSON value
  createdMs: string; // int64 -> string
  modifiedMs: string; // int64 -> string
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
  durationMs: string; // int64 -> string
  pollMs: string; // int64 -> string
}

/**
 * ClaimResponse from a claim attempt.
 */
export interface ClaimResponse {
  task?: Task;
}

/**
 * DocID identifies a specific version of a doc.
 */
export interface DocID {
  namespace: string;
  id: string;
  version: number;
}

/**
 * DocData holds the data portion of a doc, used for insertions.
 * atMs controls claim behavior on changes: "0" (or omitted) releases the
 * claim; a future ms timestamp sets the caller as claimant until then.
 */
export interface DocData {
  namespace: string;
  id?: string;
  key: string;
  secondaryKey?: string;
  content?: any;
  atMs?: string; // int64 -> string; future = claim/renew, "0" = release
  createdMs?: string; // for journal replay only
  modifiedMs?: string; // for journal replay only
}

/**
 * DocChange identifies a doc by DocID and provides updated data.
 */
export interface DocChange {
  oldId: DocID;
  newData: DocData;
}

/**
 * Doc is a complete doc object.
 */
export interface Doc {
  namespace: string;
  id: string;
  version: number;
  claimant: string;
  atMs: string; // int64 -> string
  key: string;
  secondaryKey: string;
  content?: any;
  createdMs: string; // int64 -> string
  modifiedMs: string; // int64 -> string
}

/**
 * DocQuery describes a listing request for docs in a namespace.
 * If ids is non-empty, key range and limit are ignored.
 */
export interface DocQuery {
  namespace: string;
  keyStart?: string;
  keyEnd?: string;
  limit?: number;
  omitValues?: boolean;
  ids?: string[];
}

/**
 * DocClaim describes an atomic all-or-nothing claim of docs sharing a key.
 */
export interface DocClaim {
  namespace: string;
  key: string;
  durationMs?: string; // int64 -> string; defaults to DefaultClaimDuration
  claimant?: string;   // normally auto-set by the client
}

/**
 * DocsRequest asks for a listing of docs matching a query.
 */
export interface DocsRequest {
  query: DocQuery;
}

/**
 * DocsResponse contains the listed docs.
 */
export interface DocsResponse {
  docs: Doc[];
}

/**
 * ClaimDocsRequest asks to atomically claim a set of docs.
 */
export interface ClaimDocsRequest {
  claimQuery: DocClaim;
}

/**
 * ClaimDocsResponse contains the claimed docs.
 */
export interface ClaimDocsResponse {
  docs: Doc[];
}

/**
 * NamespaceStat contains statistics for a single doc namespace.
 */
export interface NamespaceStat {
  name: string;
  numDocs: number;
  numClaimed: number;
}

/**
 * NamespacesRequest asks for a listing of doc namespaces.
 */
export interface NamespacesRequest {
  matchPrefix?: string[];
  matchExact?: string[];
  limit?: number;
}

/**
 * NamespacesResponse contains the requested namespace statistics.
 */
export interface NamespacesResponse {
  namespaces: NamespaceStat[];
}

/**
 * ModifyRequest to atomically update, insert, or delete tasks and docs.
 */
export interface ModifyRequest {
  claimantId: string;
  inserts?: TaskData[];
  changes?: TaskChange[];
  deletes?: TaskID[];
  depends?: TaskID[];
  docInserts?: DocData[];
  docChanges?: DocChange[];
  docDeletes?: DocID[];
  docDepends?: DocID[];
}

/**
 * ModifyResponse with results of a transaction.
 */
export interface ModifyResponse {
  inserted?: Task[];
  changed?: Task[];
  insertedDocs?: Doc[];
  changedDocs?: Doc[];
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
  timeMs: string; // int64 -> string
}

/**
 * EntroQClientInterface defines the methods required by a task worker.
 */
export interface EntroQClientInterface {
  claim(
    queues: string[],
    durationMs?: number,
    pollMs?: number
  ): Promise<ClaimResponse>;
  modify(
    request: Omit<ModifyRequest, "claimantId">
  ): Promise<ModifyResponse>;
}

/**
 * EntroQDocClientInterface extends EntroQClientInterface with doc operations,
 * required by a doc-aware worker.
 */
export interface EntroQDocClientInterface extends EntroQClientInterface {
  docs(request: DocsRequest): Promise<DocsResponse>;
  claimDocs(request: ClaimDocsRequest): Promise<ClaimDocsResponse>;
  namespaceStats(request?: NamespacesRequest): Promise<NamespacesResponse>;
}
