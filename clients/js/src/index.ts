// What a worker needs. The protocol machinery stays in ./gateway.
export type { Doc, DocID, DocSetClaim, ModifyRequest, Task, TaskID } from "./types";
export {
  claimSet,
  Context,
  FatalWorker,
  Gateway,
  GatewayError,
  M,
  Modification,
  MoveError,
  RetryError,
  Work,
  Worker,
} from "./gateway";
export type { GatewayOptions, Handlers, WorkerOptions } from "./gateway";
