// package: proto
// file: entroq.proto

import * as jspb from 'google-protobuf';

export class TaskID extends jspb.Message {
  getId(): string;
  setId(value: string): void;

  getVersion(): number;
  setVersion(value: number): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): TaskID.AsObject;
  static toObject(includeInstance: boolean, msg: TaskID): TaskID.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: TaskID, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): TaskID;
  static deserializeBinaryFromReader(message: TaskID, reader: jspb.BinaryReader): TaskID;
}

export namespace TaskID {
  export type AsObject = {
    id: string,
    version: number,
  }
}

export class TaskData extends jspb.Message {
  getQueue(): string;
  setQueue(value: string): void;

  getAtMs(): number;
  setAtMs(value: number): void;

  getValue(): Uint8Array | string;
  getValue_asU8(): Uint8Array;
  getValue_asB64(): string;
  setValue(value: Uint8Array | string): void;

  getId(): string;
  setId(value: string): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): TaskData.AsObject;
  static toObject(includeInstance: boolean, msg: TaskData): TaskData.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: TaskData, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): TaskData;
  static deserializeBinaryFromReader(message: TaskData, reader: jspb.BinaryReader): TaskData;
}

export namespace TaskData {
  export type AsObject = {
    queue: string,
    atMs: number,
    value: Uint8Array | string,
    id: string,
  }
}

export class TaskChange extends jspb.Message {
  hasOldId(): boolean;
  clearOldId(): void;
  getOldId(): TaskID | undefined;
  setOldId(value?: TaskID): void;

  hasNewData(): boolean;
  clearNewData(): void;
  getNewData(): TaskData | undefined;
  setNewData(value?: TaskData): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): TaskChange.AsObject;
  static toObject(includeInstance: boolean, msg: TaskChange): TaskChange.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: TaskChange, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): TaskChange;
  static deserializeBinaryFromReader(message: TaskChange, reader: jspb.BinaryReader): TaskChange;
}

export namespace TaskChange {
  export type AsObject = {
    oldId?: TaskID.AsObject,
    newData?: TaskData.AsObject,
  }
}

export class Task extends jspb.Message {
  getQueue(): string;
  setQueue(value: string): void;

  getId(): string;
  setId(value: string): void;

  getVersion(): number;
  setVersion(value: number): void;

  getAtMs(): number;
  setAtMs(value: number): void;

  getClaimantId(): string;
  setClaimantId(value: string): void;

  getValue(): Uint8Array | string;
  getValue_asU8(): Uint8Array;
  getValue_asB64(): string;
  setValue(value: Uint8Array | string): void;

  getCreatedMs(): number;
  setCreatedMs(value: number): void;

  getModifiedMs(): number;
  setModifiedMs(value: number): void;

  getClaims(): number;
  setClaims(value: number): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): Task.AsObject;
  static toObject(includeInstance: boolean, msg: Task): Task.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: Task, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): Task;
  static deserializeBinaryFromReader(message: Task, reader: jspb.BinaryReader): Task;
}

export namespace Task {
  export type AsObject = {
    queue: string,
    id: string,
    version: number,
    atMs: number,
    claimantId: string,
    value: Uint8Array | string,
    createdMs: number,
    modifiedMs: number,
    claims: number,
  }
}

export class QueueStats extends jspb.Message {
  getName(): string;
  setName(value: string): void;

  getNumTasks(): number;
  setNumTasks(value: number): void;

  getNumClaimed(): number;
  setNumClaimed(value: number): void;

  getNumAvailable(): number;
  setNumAvailable(value: number): void;

  getMaxClaims(): number;
  setMaxClaims(value: number): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): QueueStats.AsObject;
  static toObject(includeInstance: boolean, msg: QueueStats): QueueStats.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: QueueStats, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): QueueStats;
  static deserializeBinaryFromReader(message: QueueStats, reader: jspb.BinaryReader): QueueStats;
}

export namespace QueueStats {
  export type AsObject = {
    name: string,
    numTasks: number,
    numClaimed: number,
    numAvailable: number,
    maxClaims: number,
  }
}

export class ClaimRequest extends jspb.Message {
  getClaimantId(): string;
  setClaimantId(value: string): void;

  clearQueuesList(): void;
  getQueuesList(): Array<string>;
  setQueuesList(value: Array<string>): void;
  addQueues(value: string, index?: number): string;

  getDurationMs(): number;
  setDurationMs(value: number): void;

  getPollMs(): number;
  setPollMs(value: number): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): ClaimRequest.AsObject;
  static toObject(includeInstance: boolean, msg: ClaimRequest): ClaimRequest.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: ClaimRequest, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): ClaimRequest;
  static deserializeBinaryFromReader(message: ClaimRequest, reader: jspb.BinaryReader): ClaimRequest;
}

export namespace ClaimRequest {
  export type AsObject = {
    claimantId: string,
    queuesList: Array<string>,
    durationMs: number,
    pollMs: number,
  }
}

export class ClaimResponse extends jspb.Message {
  hasTask(): boolean;
  clearTask(): void;
  getTask(): Task | undefined;
  setTask(value?: Task): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): ClaimResponse.AsObject;
  static toObject(includeInstance: boolean, msg: ClaimResponse): ClaimResponse.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: ClaimResponse, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): ClaimResponse;
  static deserializeBinaryFromReader(message: ClaimResponse, reader: jspb.BinaryReader): ClaimResponse;
}

export namespace ClaimResponse {
  export type AsObject = {
    task?: Task.AsObject,
  }
}

export class ModifyRequest extends jspb.Message {
  getClaimantId(): string;
  setClaimantId(value: string): void;

  clearInsertsList(): void;
  getInsertsList(): Array<TaskData>;
  setInsertsList(value: Array<TaskData>): void;
  addInserts(value?: TaskData, index?: number): TaskData;

  clearChangesList(): void;
  getChangesList(): Array<TaskChange>;
  setChangesList(value: Array<TaskChange>): void;
  addChanges(value?: TaskChange, index?: number): TaskChange;

  clearDeletesList(): void;
  getDeletesList(): Array<TaskID>;
  setDeletesList(value: Array<TaskID>): void;
  addDeletes(value?: TaskID, index?: number): TaskID;

  clearDependsList(): void;
  getDependsList(): Array<TaskID>;
  setDependsList(value: Array<TaskID>): void;
  addDepends(value?: TaskID, index?: number): TaskID;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): ModifyRequest.AsObject;
  static toObject(includeInstance: boolean, msg: ModifyRequest): ModifyRequest.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: ModifyRequest, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): ModifyRequest;
  static deserializeBinaryFromReader(message: ModifyRequest, reader: jspb.BinaryReader): ModifyRequest;
}

export namespace ModifyRequest {
  export type AsObject = {
    claimantId: string,
    insertsList: Array<TaskData.AsObject>,
    changesList: Array<TaskChange.AsObject>,
    deletesList: Array<TaskID.AsObject>,
    dependsList: Array<TaskID.AsObject>,
  }
}

export class ModifyResponse extends jspb.Message {
  clearInsertedList(): void;
  getInsertedList(): Array<Task>;
  setInsertedList(value: Array<Task>): void;
  addInserted(value?: Task, index?: number): Task;

  clearChangedList(): void;
  getChangedList(): Array<Task>;
  setChangedList(value: Array<Task>): void;
  addChanged(value?: Task, index?: number): Task;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): ModifyResponse.AsObject;
  static toObject(includeInstance: boolean, msg: ModifyResponse): ModifyResponse.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: ModifyResponse, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): ModifyResponse;
  static deserializeBinaryFromReader(message: ModifyResponse, reader: jspb.BinaryReader): ModifyResponse;
}

export namespace ModifyResponse {
  export type AsObject = {
    insertedList: Array<Task.AsObject>,
    changedList: Array<Task.AsObject>,
  }
}

export class ModifyDep extends jspb.Message {
  getType(): DepType;
  setType(value: DepType): void;

  hasId(): boolean;
  clearId(): void;
  getId(): TaskID | undefined;
  setId(value?: TaskID): void;

  getMsg(): string;
  setMsg(value: string): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): ModifyDep.AsObject;
  static toObject(includeInstance: boolean, msg: ModifyDep): ModifyDep.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: ModifyDep, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): ModifyDep;
  static deserializeBinaryFromReader(message: ModifyDep, reader: jspb.BinaryReader): ModifyDep;
}

export namespace ModifyDep {
  export type AsObject = {
    type: DepType,
    id?: TaskID.AsObject,
    msg: string,
  }
}

export class TasksRequest extends jspb.Message {
  getClaimantId(): string;
  setClaimantId(value: string): void;

  getQueue(): string;
  setQueue(value: string): void;

  getLimit(): number;
  setLimit(value: number): void;

  clearTaskIdList(): void;
  getTaskIdList(): Array<string>;
  setTaskIdList(value: Array<string>): void;
  addTaskId(value: string, index?: number): string;

  getOmitValues(): boolean;
  setOmitValues(value: boolean): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): TasksRequest.AsObject;
  static toObject(includeInstance: boolean, msg: TasksRequest): TasksRequest.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: TasksRequest, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): TasksRequest;
  static deserializeBinaryFromReader(message: TasksRequest, reader: jspb.BinaryReader): TasksRequest;
}

export namespace TasksRequest {
  export type AsObject = {
    claimantId: string,
    queue: string,
    limit: number,
    taskIdList: Array<string>,
    omitValues: boolean,
  }
}

export class TasksResponse extends jspb.Message {
  clearTasksList(): void;
  getTasksList(): Array<Task>;
  setTasksList(value: Array<Task>): void;
  addTasks(value?: Task, index?: number): Task;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): TasksResponse.AsObject;
  static toObject(includeInstance: boolean, msg: TasksResponse): TasksResponse.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: TasksResponse, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): TasksResponse;
  static deserializeBinaryFromReader(message: TasksResponse, reader: jspb.BinaryReader): TasksResponse;
}

export namespace TasksResponse {
  export type AsObject = {
    tasksList: Array<Task.AsObject>,
  }
}

export class QueuesRequest extends jspb.Message {
  clearMatchPrefixList(): void;
  getMatchPrefixList(): Array<string>;
  setMatchPrefixList(value: Array<string>): void;
  addMatchPrefix(value: string, index?: number): string;

  clearMatchExactList(): void;
  getMatchExactList(): Array<string>;
  setMatchExactList(value: Array<string>): void;
  addMatchExact(value: string, index?: number): string;

  getLimit(): number;
  setLimit(value: number): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): QueuesRequest.AsObject;
  static toObject(includeInstance: boolean, msg: QueuesRequest): QueuesRequest.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: QueuesRequest, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): QueuesRequest;
  static deserializeBinaryFromReader(message: QueuesRequest, reader: jspb.BinaryReader): QueuesRequest;
}

export namespace QueuesRequest {
  export type AsObject = {
    matchPrefixList: Array<string>,
    matchExactList: Array<string>,
    limit: number,
  }
}

export class QueuesResponse extends jspb.Message {
  clearQueuesList(): void;
  getQueuesList(): Array<QueueStats>;
  setQueuesList(value: Array<QueueStats>): void;
  addQueues(value?: QueueStats, index?: number): QueueStats;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): QueuesResponse.AsObject;
  static toObject(includeInstance: boolean, msg: QueuesResponse): QueuesResponse.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: QueuesResponse, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): QueuesResponse;
  static deserializeBinaryFromReader(message: QueuesResponse, reader: jspb.BinaryReader): QueuesResponse;
}

export namespace QueuesResponse {
  export type AsObject = {
    queuesList: Array<QueueStats.AsObject>,
  }
}

export class TimeRequest extends jspb.Message {
  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): TimeRequest.AsObject;
  static toObject(includeInstance: boolean, msg: TimeRequest): TimeRequest.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: TimeRequest, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): TimeRequest;
  static deserializeBinaryFromReader(message: TimeRequest, reader: jspb.BinaryReader): TimeRequest;
}

export namespace TimeRequest {
  export type AsObject = {
  }
}

export class TimeResponse extends jspb.Message {
  getTimeMs(): number;
  setTimeMs(value: number): void;

  serializeBinary(): Uint8Array;
  toObject(includeInstance?: boolean): TimeResponse.AsObject;
  static toObject(includeInstance: boolean, msg: TimeResponse): TimeResponse.AsObject;
  static extensions: {[key: number]: jspb.ExtensionFieldInfo<jspb.Message>};
  static extensionsBinary: {[key: number]: jspb.ExtensionFieldBinaryInfo<jspb.Message>};
  static serializeBinaryToWriter(message: TimeResponse, writer: jspb.BinaryWriter): void;
  static deserializeBinary(bytes: Uint8Array): TimeResponse;
  static deserializeBinaryFromReader(message: TimeResponse, reader: jspb.BinaryReader): TimeResponse;
}

export namespace TimeResponse {
  export type AsObject = {
    timeMs: number,
  }
}

export enum DepType {
  CLAIM = 0,
  DELETE = 1,
  CHANGE = 2,
  DEPEND = 3,
  DETAIL = 4,
  INSERT = 5,
}

