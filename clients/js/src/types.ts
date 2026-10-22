/**
 * The EntroQ objects a worker sees: the protojson of the api/entroq.proto
 * messages of the same names, written out by hand with only the fields a
 * worker uses. int64 fields arrive as strings, and either strings or numbers
 * are accepted.
 */

/** A task, as the gateway hands it over. */
export interface Task {
  queue: string;
  id: string;
  version: number;
  atMs: string;
  claimantId: string;
  value: any;
  createdMs: string;
  modifiedMs: string;
  claims: number;
  attempt: number;
  err: string;
}

/** A task named at a version. */
export interface TaskID {
  queue: string;
  id: string;
  version: number;
}

/** A doc, or a doc set: a set has no id or content, and len counts its docs. */
export interface Doc {
  namespace: string;
  id: string;
  version: number;
  claimant: string;
  atMs: string;
  key: string;
  secondaryKey: string;
  content?: any;
  createdMs: string;
  modifiedMs: string;
  len?: number;
}

/** A doc named at a version, by id; or a doc set, by key. */
export interface DocID {
  namespace: string;
  id?: string;
  key?: string;
  version: number;
}

/** A doc set to claim, by key, for `take`. */
export interface DocSetClaim {
  set: DocID;
  omitMembers?: boolean;
}

/** The operations of one commit, by the ModifyRequest field each goes in. */
export interface ModifyRequest {
  inserts?: any[];
  changes?: any[];
  deletes?: TaskID[];
  depends?: TaskID[];
  docInserts?: any[];
  docChanges?: any[];
  docDeletes?: DocID[];
  docDepends?: DocID[];
}
