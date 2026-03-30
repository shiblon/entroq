import * as $protobuf from "protobufjs";
/** Namespace proto. */
export namespace proto {

    /** Properties of a TaskID. */
    interface ITaskID {

        /** TaskID id */
        id?: (string|null);

        /** TaskID version */
        version?: (number|null);

        /** TaskID queue */
        queue?: (string|null);
    }

    /** Represents a TaskID. */
    class TaskID implements ITaskID {

        /**
         * Constructs a new TaskID.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.ITaskID);

        /** TaskID id. */
        public id: string;

        /** TaskID version. */
        public version: number;

        /** TaskID queue. */
        public queue: string;

        /**
         * Creates a new TaskID instance using the specified properties.
         * @param [properties] Properties to set
         * @returns TaskID instance
         */
        public static create(properties?: proto.ITaskID): proto.TaskID;

        /**
         * Encodes the specified TaskID message. Does not implicitly {@link proto.TaskID.verify|verify} messages.
         * @param message TaskID message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.ITaskID, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified TaskID message, length delimited. Does not implicitly {@link proto.TaskID.verify|verify} messages.
         * @param message TaskID message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.ITaskID, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a TaskID message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns TaskID
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.TaskID;

        /**
         * Decodes a TaskID message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns TaskID
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.TaskID;

        /**
         * Verifies a TaskID message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a TaskID message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns TaskID
         */
        public static fromObject(object: { [k: string]: any }): proto.TaskID;

        /**
         * Creates a plain object from a TaskID message. Also converts values to other types if specified.
         * @param message TaskID
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.TaskID, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this TaskID to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a TaskData. */
    interface ITaskData {

        /** TaskData queue */
        queue?: (string|null);

        /** TaskData atMs */
        atMs?: (number|Long|null);

        /** TaskData value */
        value?: (Uint8Array|null);

        /** TaskData id */
        id?: (string|null);

        /** TaskData attempt */
        attempt?: (number|null);

        /** TaskData err */
        err?: (string|null);
    }

    /** Represents a TaskData. */
    class TaskData implements ITaskData {

        /**
         * Constructs a new TaskData.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.ITaskData);

        /** TaskData queue. */
        public queue: string;

        /** TaskData atMs. */
        public atMs: (number|Long);

        /** TaskData value. */
        public value: Uint8Array;

        /** TaskData id. */
        public id: string;

        /** TaskData attempt. */
        public attempt: number;

        /** TaskData err. */
        public err: string;

        /**
         * Creates a new TaskData instance using the specified properties.
         * @param [properties] Properties to set
         * @returns TaskData instance
         */
        public static create(properties?: proto.ITaskData): proto.TaskData;

        /**
         * Encodes the specified TaskData message. Does not implicitly {@link proto.TaskData.verify|verify} messages.
         * @param message TaskData message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.ITaskData, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified TaskData message, length delimited. Does not implicitly {@link proto.TaskData.verify|verify} messages.
         * @param message TaskData message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.ITaskData, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a TaskData message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns TaskData
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.TaskData;

        /**
         * Decodes a TaskData message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns TaskData
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.TaskData;

        /**
         * Verifies a TaskData message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a TaskData message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns TaskData
         */
        public static fromObject(object: { [k: string]: any }): proto.TaskData;

        /**
         * Creates a plain object from a TaskData message. Also converts values to other types if specified.
         * @param message TaskData
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.TaskData, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this TaskData to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a TaskChange. */
    interface ITaskChange {

        /** TaskChange oldId */
        oldId?: (proto.ITaskID|null);

        /** TaskChange newData */
        newData?: (proto.ITaskData|null);
    }

    /** Represents a TaskChange. */
    class TaskChange implements ITaskChange {

        /**
         * Constructs a new TaskChange.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.ITaskChange);

        /** TaskChange oldId. */
        public oldId?: (proto.ITaskID|null);

        /** TaskChange newData. */
        public newData?: (proto.ITaskData|null);

        /**
         * Creates a new TaskChange instance using the specified properties.
         * @param [properties] Properties to set
         * @returns TaskChange instance
         */
        public static create(properties?: proto.ITaskChange): proto.TaskChange;

        /**
         * Encodes the specified TaskChange message. Does not implicitly {@link proto.TaskChange.verify|verify} messages.
         * @param message TaskChange message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.ITaskChange, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified TaskChange message, length delimited. Does not implicitly {@link proto.TaskChange.verify|verify} messages.
         * @param message TaskChange message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.ITaskChange, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a TaskChange message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns TaskChange
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.TaskChange;

        /**
         * Decodes a TaskChange message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns TaskChange
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.TaskChange;

        /**
         * Verifies a TaskChange message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a TaskChange message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns TaskChange
         */
        public static fromObject(object: { [k: string]: any }): proto.TaskChange;

        /**
         * Creates a plain object from a TaskChange message. Also converts values to other types if specified.
         * @param message TaskChange
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.TaskChange, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this TaskChange to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a Task. */
    interface ITask {

        /** Task queue */
        queue?: (string|null);

        /** Task id */
        id?: (string|null);

        /** Task version */
        version?: (number|null);

        /** Task atMs */
        atMs?: (number|Long|null);

        /** Task claimantId */
        claimantId?: (string|null);

        /** Task value */
        value?: (Uint8Array|null);

        /** Task createdMs */
        createdMs?: (number|Long|null);

        /** Task modifiedMs */
        modifiedMs?: (number|Long|null);

        /** Task claims */
        claims?: (number|null);

        /** Task attempt */
        attempt?: (number|null);

        /** Task err */
        err?: (string|null);
    }

    /** Represents a Task. */
    class Task implements ITask {

        /**
         * Constructs a new Task.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.ITask);

        /** Task queue. */
        public queue: string;

        /** Task id. */
        public id: string;

        /** Task version. */
        public version: number;

        /** Task atMs. */
        public atMs: (number|Long);

        /** Task claimantId. */
        public claimantId: string;

        /** Task value. */
        public value: Uint8Array;

        /** Task createdMs. */
        public createdMs: (number|Long);

        /** Task modifiedMs. */
        public modifiedMs: (number|Long);

        /** Task claims. */
        public claims: number;

        /** Task attempt. */
        public attempt: number;

        /** Task err. */
        public err: string;

        /**
         * Creates a new Task instance using the specified properties.
         * @param [properties] Properties to set
         * @returns Task instance
         */
        public static create(properties?: proto.ITask): proto.Task;

        /**
         * Encodes the specified Task message. Does not implicitly {@link proto.Task.verify|verify} messages.
         * @param message Task message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.ITask, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified Task message, length delimited. Does not implicitly {@link proto.Task.verify|verify} messages.
         * @param message Task message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.ITask, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a Task message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns Task
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.Task;

        /**
         * Decodes a Task message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns Task
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.Task;

        /**
         * Verifies a Task message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a Task message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns Task
         */
        public static fromObject(object: { [k: string]: any }): proto.Task;

        /**
         * Creates a plain object from a Task message. Also converts values to other types if specified.
         * @param message Task
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.Task, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this Task to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a QueueStats. */
    interface IQueueStats {

        /** QueueStats name */
        name?: (string|null);

        /** QueueStats numTasks */
        numTasks?: (number|null);

        /** QueueStats numClaimed */
        numClaimed?: (number|null);

        /** QueueStats numAvailable */
        numAvailable?: (number|null);

        /** QueueStats maxClaims */
        maxClaims?: (number|null);
    }

    /** Represents a QueueStats. */
    class QueueStats implements IQueueStats {

        /**
         * Constructs a new QueueStats.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IQueueStats);

        /** QueueStats name. */
        public name: string;

        /** QueueStats numTasks. */
        public numTasks: number;

        /** QueueStats numClaimed. */
        public numClaimed: number;

        /** QueueStats numAvailable. */
        public numAvailable: number;

        /** QueueStats maxClaims. */
        public maxClaims: number;

        /**
         * Creates a new QueueStats instance using the specified properties.
         * @param [properties] Properties to set
         * @returns QueueStats instance
         */
        public static create(properties?: proto.IQueueStats): proto.QueueStats;

        /**
         * Encodes the specified QueueStats message. Does not implicitly {@link proto.QueueStats.verify|verify} messages.
         * @param message QueueStats message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IQueueStats, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified QueueStats message, length delimited. Does not implicitly {@link proto.QueueStats.verify|verify} messages.
         * @param message QueueStats message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IQueueStats, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a QueueStats message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns QueueStats
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.QueueStats;

        /**
         * Decodes a QueueStats message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns QueueStats
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.QueueStats;

        /**
         * Verifies a QueueStats message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a QueueStats message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns QueueStats
         */
        public static fromObject(object: { [k: string]: any }): proto.QueueStats;

        /**
         * Creates a plain object from a QueueStats message. Also converts values to other types if specified.
         * @param message QueueStats
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.QueueStats, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this QueueStats to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a ClaimRequest. */
    interface IClaimRequest {

        /** ClaimRequest claimantId */
        claimantId?: (string|null);

        /** ClaimRequest queues */
        queues?: (string[]|null);

        /** ClaimRequest durationMs */
        durationMs?: (number|Long|null);

        /** ClaimRequest pollMs */
        pollMs?: (number|Long|null);
    }

    /** Represents a ClaimRequest. */
    class ClaimRequest implements IClaimRequest {

        /**
         * Constructs a new ClaimRequest.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IClaimRequest);

        /** ClaimRequest claimantId. */
        public claimantId: string;

        /** ClaimRequest queues. */
        public queues: string[];

        /** ClaimRequest durationMs. */
        public durationMs: (number|Long);

        /** ClaimRequest pollMs. */
        public pollMs: (number|Long);

        /**
         * Creates a new ClaimRequest instance using the specified properties.
         * @param [properties] Properties to set
         * @returns ClaimRequest instance
         */
        public static create(properties?: proto.IClaimRequest): proto.ClaimRequest;

        /**
         * Encodes the specified ClaimRequest message. Does not implicitly {@link proto.ClaimRequest.verify|verify} messages.
         * @param message ClaimRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IClaimRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified ClaimRequest message, length delimited. Does not implicitly {@link proto.ClaimRequest.verify|verify} messages.
         * @param message ClaimRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IClaimRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a ClaimRequest message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns ClaimRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.ClaimRequest;

        /**
         * Decodes a ClaimRequest message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns ClaimRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.ClaimRequest;

        /**
         * Verifies a ClaimRequest message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a ClaimRequest message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns ClaimRequest
         */
        public static fromObject(object: { [k: string]: any }): proto.ClaimRequest;

        /**
         * Creates a plain object from a ClaimRequest message. Also converts values to other types if specified.
         * @param message ClaimRequest
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.ClaimRequest, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this ClaimRequest to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a ClaimResponse. */
    interface IClaimResponse {

        /** ClaimResponse task */
        task?: (proto.ITask|null);
    }

    /** Represents a ClaimResponse. */
    class ClaimResponse implements IClaimResponse {

        /**
         * Constructs a new ClaimResponse.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IClaimResponse);

        /** ClaimResponse task. */
        public task?: (proto.ITask|null);

        /**
         * Creates a new ClaimResponse instance using the specified properties.
         * @param [properties] Properties to set
         * @returns ClaimResponse instance
         */
        public static create(properties?: proto.IClaimResponse): proto.ClaimResponse;

        /**
         * Encodes the specified ClaimResponse message. Does not implicitly {@link proto.ClaimResponse.verify|verify} messages.
         * @param message ClaimResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IClaimResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified ClaimResponse message, length delimited. Does not implicitly {@link proto.ClaimResponse.verify|verify} messages.
         * @param message ClaimResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IClaimResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a ClaimResponse message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns ClaimResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.ClaimResponse;

        /**
         * Decodes a ClaimResponse message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns ClaimResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.ClaimResponse;

        /**
         * Verifies a ClaimResponse message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a ClaimResponse message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns ClaimResponse
         */
        public static fromObject(object: { [k: string]: any }): proto.ClaimResponse;

        /**
         * Creates a plain object from a ClaimResponse message. Also converts values to other types if specified.
         * @param message ClaimResponse
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.ClaimResponse, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this ClaimResponse to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a ModifyRequest. */
    interface IModifyRequest {

        /** ModifyRequest claimantId */
        claimantId?: (string|null);

        /** ModifyRequest inserts */
        inserts?: (proto.ITaskData[]|null);

        /** ModifyRequest changes */
        changes?: (proto.ITaskChange[]|null);

        /** ModifyRequest deletes */
        deletes?: (proto.ITaskID[]|null);

        /** ModifyRequest depends */
        depends?: (proto.ITaskID[]|null);
    }

    /** Represents a ModifyRequest. */
    class ModifyRequest implements IModifyRequest {

        /**
         * Constructs a new ModifyRequest.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IModifyRequest);

        /** ModifyRequest claimantId. */
        public claimantId: string;

        /** ModifyRequest inserts. */
        public inserts: proto.ITaskData[];

        /** ModifyRequest changes. */
        public changes: proto.ITaskChange[];

        /** ModifyRequest deletes. */
        public deletes: proto.ITaskID[];

        /** ModifyRequest depends. */
        public depends: proto.ITaskID[];

        /**
         * Creates a new ModifyRequest instance using the specified properties.
         * @param [properties] Properties to set
         * @returns ModifyRequest instance
         */
        public static create(properties?: proto.IModifyRequest): proto.ModifyRequest;

        /**
         * Encodes the specified ModifyRequest message. Does not implicitly {@link proto.ModifyRequest.verify|verify} messages.
         * @param message ModifyRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IModifyRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified ModifyRequest message, length delimited. Does not implicitly {@link proto.ModifyRequest.verify|verify} messages.
         * @param message ModifyRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IModifyRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a ModifyRequest message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns ModifyRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.ModifyRequest;

        /**
         * Decodes a ModifyRequest message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns ModifyRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.ModifyRequest;

        /**
         * Verifies a ModifyRequest message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a ModifyRequest message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns ModifyRequest
         */
        public static fromObject(object: { [k: string]: any }): proto.ModifyRequest;

        /**
         * Creates a plain object from a ModifyRequest message. Also converts values to other types if specified.
         * @param message ModifyRequest
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.ModifyRequest, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this ModifyRequest to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a ModifyResponse. */
    interface IModifyResponse {

        /** ModifyResponse inserted */
        inserted?: (proto.ITask[]|null);

        /** ModifyResponse changed */
        changed?: (proto.ITask[]|null);
    }

    /** Represents a ModifyResponse. */
    class ModifyResponse implements IModifyResponse {

        /**
         * Constructs a new ModifyResponse.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IModifyResponse);

        /** ModifyResponse inserted. */
        public inserted: proto.ITask[];

        /** ModifyResponse changed. */
        public changed: proto.ITask[];

        /**
         * Creates a new ModifyResponse instance using the specified properties.
         * @param [properties] Properties to set
         * @returns ModifyResponse instance
         */
        public static create(properties?: proto.IModifyResponse): proto.ModifyResponse;

        /**
         * Encodes the specified ModifyResponse message. Does not implicitly {@link proto.ModifyResponse.verify|verify} messages.
         * @param message ModifyResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IModifyResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified ModifyResponse message, length delimited. Does not implicitly {@link proto.ModifyResponse.verify|verify} messages.
         * @param message ModifyResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IModifyResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a ModifyResponse message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns ModifyResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.ModifyResponse;

        /**
         * Decodes a ModifyResponse message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns ModifyResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.ModifyResponse;

        /**
         * Verifies a ModifyResponse message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a ModifyResponse message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns ModifyResponse
         */
        public static fromObject(object: { [k: string]: any }): proto.ModifyResponse;

        /**
         * Creates a plain object from a ModifyResponse message. Also converts values to other types if specified.
         * @param message ModifyResponse
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.ModifyResponse, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this ModifyResponse to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** ActionType enum. */
    enum ActionType {
        CLAIM = 0,
        DELETE = 1,
        CHANGE = 2,
        DEPEND = 3,
        DETAIL = 4,
        INSERT = 5,
        READ = 6
    }

    /** Properties of a ModifyDep. */
    interface IModifyDep {

        /** ModifyDep type */
        type?: (proto.ActionType|null);

        /** ModifyDep id */
        id?: (proto.ITaskID|null);

        /** ModifyDep msg */
        msg?: (string|null);
    }

    /** Represents a ModifyDep. */
    class ModifyDep implements IModifyDep {

        /**
         * Constructs a new ModifyDep.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IModifyDep);

        /** ModifyDep type. */
        public type: proto.ActionType;

        /** ModifyDep id. */
        public id?: (proto.ITaskID|null);

        /** ModifyDep msg. */
        public msg: string;

        /**
         * Creates a new ModifyDep instance using the specified properties.
         * @param [properties] Properties to set
         * @returns ModifyDep instance
         */
        public static create(properties?: proto.IModifyDep): proto.ModifyDep;

        /**
         * Encodes the specified ModifyDep message. Does not implicitly {@link proto.ModifyDep.verify|verify} messages.
         * @param message ModifyDep message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IModifyDep, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified ModifyDep message, length delimited. Does not implicitly {@link proto.ModifyDep.verify|verify} messages.
         * @param message ModifyDep message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IModifyDep, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a ModifyDep message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns ModifyDep
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.ModifyDep;

        /**
         * Decodes a ModifyDep message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns ModifyDep
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.ModifyDep;

        /**
         * Verifies a ModifyDep message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a ModifyDep message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns ModifyDep
         */
        public static fromObject(object: { [k: string]: any }): proto.ModifyDep;

        /**
         * Creates a plain object from a ModifyDep message. Also converts values to other types if specified.
         * @param message ModifyDep
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.ModifyDep, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this ModifyDep to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of an AuthzDep. */
    interface IAuthzDep {

        /** AuthzDep actions */
        actions?: (proto.ActionType[]|null);

        /** AuthzDep exact */
        exact?: (string|null);

        /** AuthzDep prefix */
        prefix?: (string|null);

        /** AuthzDep msg */
        msg?: (string|null);
    }

    /** Represents an AuthzDep. */
    class AuthzDep implements IAuthzDep {

        /**
         * Constructs a new AuthzDep.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IAuthzDep);

        /** AuthzDep actions. */
        public actions: proto.ActionType[];

        /** AuthzDep exact. */
        public exact: string;

        /** AuthzDep prefix. */
        public prefix: string;

        /** AuthzDep msg. */
        public msg: string;

        /**
         * Creates a new AuthzDep instance using the specified properties.
         * @param [properties] Properties to set
         * @returns AuthzDep instance
         */
        public static create(properties?: proto.IAuthzDep): proto.AuthzDep;

        /**
         * Encodes the specified AuthzDep message. Does not implicitly {@link proto.AuthzDep.verify|verify} messages.
         * @param message AuthzDep message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IAuthzDep, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified AuthzDep message, length delimited. Does not implicitly {@link proto.AuthzDep.verify|verify} messages.
         * @param message AuthzDep message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IAuthzDep, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes an AuthzDep message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns AuthzDep
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.AuthzDep;

        /**
         * Decodes an AuthzDep message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns AuthzDep
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.AuthzDep;

        /**
         * Verifies an AuthzDep message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates an AuthzDep message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns AuthzDep
         */
        public static fromObject(object: { [k: string]: any }): proto.AuthzDep;

        /**
         * Creates a plain object from an AuthzDep message. Also converts values to other types if specified.
         * @param message AuthzDep
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.AuthzDep, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this AuthzDep to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a TasksRequest. */
    interface ITasksRequest {

        /** TasksRequest claimantId */
        claimantId?: (string|null);

        /** TasksRequest queue */
        queue?: (string|null);

        /** TasksRequest limit */
        limit?: (number|null);

        /** TasksRequest taskId */
        taskId?: (string[]|null);

        /** TasksRequest omitValues */
        omitValues?: (boolean|null);
    }

    /** Represents a TasksRequest. */
    class TasksRequest implements ITasksRequest {

        /**
         * Constructs a new TasksRequest.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.ITasksRequest);

        /** TasksRequest claimantId. */
        public claimantId: string;

        /** TasksRequest queue. */
        public queue: string;

        /** TasksRequest limit. */
        public limit: number;

        /** TasksRequest taskId. */
        public taskId: string[];

        /** TasksRequest omitValues. */
        public omitValues: boolean;

        /**
         * Creates a new TasksRequest instance using the specified properties.
         * @param [properties] Properties to set
         * @returns TasksRequest instance
         */
        public static create(properties?: proto.ITasksRequest): proto.TasksRequest;

        /**
         * Encodes the specified TasksRequest message. Does not implicitly {@link proto.TasksRequest.verify|verify} messages.
         * @param message TasksRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.ITasksRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified TasksRequest message, length delimited. Does not implicitly {@link proto.TasksRequest.verify|verify} messages.
         * @param message TasksRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.ITasksRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a TasksRequest message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns TasksRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.TasksRequest;

        /**
         * Decodes a TasksRequest message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns TasksRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.TasksRequest;

        /**
         * Verifies a TasksRequest message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a TasksRequest message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns TasksRequest
         */
        public static fromObject(object: { [k: string]: any }): proto.TasksRequest;

        /**
         * Creates a plain object from a TasksRequest message. Also converts values to other types if specified.
         * @param message TasksRequest
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.TasksRequest, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this TasksRequest to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a TasksResponse. */
    interface ITasksResponse {

        /** TasksResponse tasks */
        tasks?: (proto.ITask[]|null);
    }

    /** Represents a TasksResponse. */
    class TasksResponse implements ITasksResponse {

        /**
         * Constructs a new TasksResponse.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.ITasksResponse);

        /** TasksResponse tasks. */
        public tasks: proto.ITask[];

        /**
         * Creates a new TasksResponse instance using the specified properties.
         * @param [properties] Properties to set
         * @returns TasksResponse instance
         */
        public static create(properties?: proto.ITasksResponse): proto.TasksResponse;

        /**
         * Encodes the specified TasksResponse message. Does not implicitly {@link proto.TasksResponse.verify|verify} messages.
         * @param message TasksResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.ITasksResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified TasksResponse message, length delimited. Does not implicitly {@link proto.TasksResponse.verify|verify} messages.
         * @param message TasksResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.ITasksResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a TasksResponse message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns TasksResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.TasksResponse;

        /**
         * Decodes a TasksResponse message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns TasksResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.TasksResponse;

        /**
         * Verifies a TasksResponse message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a TasksResponse message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns TasksResponse
         */
        public static fromObject(object: { [k: string]: any }): proto.TasksResponse;

        /**
         * Creates a plain object from a TasksResponse message. Also converts values to other types if specified.
         * @param message TasksResponse
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.TasksResponse, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this TasksResponse to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a QueuesRequest. */
    interface IQueuesRequest {

        /** QueuesRequest matchPrefix */
        matchPrefix?: (string[]|null);

        /** QueuesRequest matchExact */
        matchExact?: (string[]|null);

        /** QueuesRequest limit */
        limit?: (number|null);
    }

    /** Represents a QueuesRequest. */
    class QueuesRequest implements IQueuesRequest {

        /**
         * Constructs a new QueuesRequest.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IQueuesRequest);

        /** QueuesRequest matchPrefix. */
        public matchPrefix: string[];

        /** QueuesRequest matchExact. */
        public matchExact: string[];

        /** QueuesRequest limit. */
        public limit: number;

        /**
         * Creates a new QueuesRequest instance using the specified properties.
         * @param [properties] Properties to set
         * @returns QueuesRequest instance
         */
        public static create(properties?: proto.IQueuesRequest): proto.QueuesRequest;

        /**
         * Encodes the specified QueuesRequest message. Does not implicitly {@link proto.QueuesRequest.verify|verify} messages.
         * @param message QueuesRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IQueuesRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified QueuesRequest message, length delimited. Does not implicitly {@link proto.QueuesRequest.verify|verify} messages.
         * @param message QueuesRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IQueuesRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a QueuesRequest message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns QueuesRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.QueuesRequest;

        /**
         * Decodes a QueuesRequest message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns QueuesRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.QueuesRequest;

        /**
         * Verifies a QueuesRequest message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a QueuesRequest message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns QueuesRequest
         */
        public static fromObject(object: { [k: string]: any }): proto.QueuesRequest;

        /**
         * Creates a plain object from a QueuesRequest message. Also converts values to other types if specified.
         * @param message QueuesRequest
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.QueuesRequest, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this QueuesRequest to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a QueuesResponse. */
    interface IQueuesResponse {

        /** QueuesResponse queues */
        queues?: (proto.IQueueStats[]|null);
    }

    /** Represents a QueuesResponse. */
    class QueuesResponse implements IQueuesResponse {

        /**
         * Constructs a new QueuesResponse.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.IQueuesResponse);

        /** QueuesResponse queues. */
        public queues: proto.IQueueStats[];

        /**
         * Creates a new QueuesResponse instance using the specified properties.
         * @param [properties] Properties to set
         * @returns QueuesResponse instance
         */
        public static create(properties?: proto.IQueuesResponse): proto.QueuesResponse;

        /**
         * Encodes the specified QueuesResponse message. Does not implicitly {@link proto.QueuesResponse.verify|verify} messages.
         * @param message QueuesResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.IQueuesResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified QueuesResponse message, length delimited. Does not implicitly {@link proto.QueuesResponse.verify|verify} messages.
         * @param message QueuesResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.IQueuesResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a QueuesResponse message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns QueuesResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.QueuesResponse;

        /**
         * Decodes a QueuesResponse message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns QueuesResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.QueuesResponse;

        /**
         * Verifies a QueuesResponse message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a QueuesResponse message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns QueuesResponse
         */
        public static fromObject(object: { [k: string]: any }): proto.QueuesResponse;

        /**
         * Creates a plain object from a QueuesResponse message. Also converts values to other types if specified.
         * @param message QueuesResponse
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.QueuesResponse, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this QueuesResponse to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a TimeRequest. */
    interface ITimeRequest {
    }

    /** Represents a TimeRequest. */
    class TimeRequest implements ITimeRequest {

        /**
         * Constructs a new TimeRequest.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.ITimeRequest);

        /**
         * Creates a new TimeRequest instance using the specified properties.
         * @param [properties] Properties to set
         * @returns TimeRequest instance
         */
        public static create(properties?: proto.ITimeRequest): proto.TimeRequest;

        /**
         * Encodes the specified TimeRequest message. Does not implicitly {@link proto.TimeRequest.verify|verify} messages.
         * @param message TimeRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.ITimeRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified TimeRequest message, length delimited. Does not implicitly {@link proto.TimeRequest.verify|verify} messages.
         * @param message TimeRequest message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.ITimeRequest, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a TimeRequest message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns TimeRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.TimeRequest;

        /**
         * Decodes a TimeRequest message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns TimeRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.TimeRequest;

        /**
         * Verifies a TimeRequest message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a TimeRequest message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns TimeRequest
         */
        public static fromObject(object: { [k: string]: any }): proto.TimeRequest;

        /**
         * Creates a plain object from a TimeRequest message. Also converts values to other types if specified.
         * @param message TimeRequest
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.TimeRequest, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this TimeRequest to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Properties of a TimeResponse. */
    interface ITimeResponse {

        /** TimeResponse timeMs */
        timeMs?: (number|Long|null);
    }

    /** Represents a TimeResponse. */
    class TimeResponse implements ITimeResponse {

        /**
         * Constructs a new TimeResponse.
         * @param [properties] Properties to set
         */
        constructor(properties?: proto.ITimeResponse);

        /** TimeResponse timeMs. */
        public timeMs: (number|Long);

        /**
         * Creates a new TimeResponse instance using the specified properties.
         * @param [properties] Properties to set
         * @returns TimeResponse instance
         */
        public static create(properties?: proto.ITimeResponse): proto.TimeResponse;

        /**
         * Encodes the specified TimeResponse message. Does not implicitly {@link proto.TimeResponse.verify|verify} messages.
         * @param message TimeResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encode(message: proto.ITimeResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Encodes the specified TimeResponse message, length delimited. Does not implicitly {@link proto.TimeResponse.verify|verify} messages.
         * @param message TimeResponse message or plain object to encode
         * @param [writer] Writer to encode to
         * @returns Writer
         */
        public static encodeDelimited(message: proto.ITimeResponse, writer?: $protobuf.Writer): $protobuf.Writer;

        /**
         * Decodes a TimeResponse message from the specified reader or buffer.
         * @param reader Reader or buffer to decode from
         * @param [length] Message length if known beforehand
         * @returns TimeResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decode(reader: ($protobuf.Reader|Uint8Array), length?: number): proto.TimeResponse;

        /**
         * Decodes a TimeResponse message from the specified reader or buffer, length delimited.
         * @param reader Reader or buffer to decode from
         * @returns TimeResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        public static decodeDelimited(reader: ($protobuf.Reader|Uint8Array)): proto.TimeResponse;

        /**
         * Verifies a TimeResponse message.
         * @param message Plain object to verify
         * @returns `null` if valid, otherwise the reason why it is not
         */
        public static verify(message: { [k: string]: any }): (string|null);

        /**
         * Creates a TimeResponse message from a plain object. Also converts values to their respective internal types.
         * @param object Plain object
         * @returns TimeResponse
         */
        public static fromObject(object: { [k: string]: any }): proto.TimeResponse;

        /**
         * Creates a plain object from a TimeResponse message. Also converts values to other types if specified.
         * @param message TimeResponse
         * @param [options] Conversion options
         * @returns Plain object
         */
        public static toObject(message: proto.TimeResponse, options?: $protobuf.IConversionOptions): { [k: string]: any };

        /**
         * Converts this TimeResponse to JSON.
         * @returns JSON object
         */
        public toJSON(): { [k: string]: any };
    }

    /** Represents an EntroQ */
    class EntroQ extends $protobuf.rpc.Service {

        /**
         * Constructs a new EntroQ service.
         * @param rpcImpl RPC implementation
         * @param [requestDelimited=false] Whether requests are length-delimited
         * @param [responseDelimited=false] Whether responses are length-delimited
         */
        constructor(rpcImpl: $protobuf.RPCImpl, requestDelimited?: boolean, responseDelimited?: boolean);

        /**
         * Creates new EntroQ service using the specified rpc implementation.
         * @param rpcImpl RPC implementation
         * @param [requestDelimited=false] Whether requests are length-delimited
         * @param [responseDelimited=false] Whether responses are length-delimited
         * @returns RPC service. Useful where requests and/or responses are streamed.
         */
        public static create(rpcImpl: $protobuf.RPCImpl, requestDelimited?: boolean, responseDelimited?: boolean): EntroQ;

        /**
         * Calls TryClaim.
         * @param request ClaimRequest message or plain object
         * @param callback Node-style callback called with the error, if any, and ClaimResponse
         */
        public tryClaim(request: proto.IClaimRequest, callback: proto.EntroQ.TryClaimCallback): void;

        /**
         * Calls TryClaim.
         * @param request ClaimRequest message or plain object
         * @returns Promise
         */
        public tryClaim(request: proto.IClaimRequest): Promise<proto.ClaimResponse>;

        /**
         * Calls Claim.
         * @param request ClaimRequest message or plain object
         * @param callback Node-style callback called with the error, if any, and ClaimResponse
         */
        public claim(request: proto.IClaimRequest, callback: proto.EntroQ.ClaimCallback): void;

        /**
         * Calls Claim.
         * @param request ClaimRequest message or plain object
         * @returns Promise
         */
        public claim(request: proto.IClaimRequest): Promise<proto.ClaimResponse>;

        /**
         * Calls Modify.
         * @param request ModifyRequest message or plain object
         * @param callback Node-style callback called with the error, if any, and ModifyResponse
         */
        public modify(request: proto.IModifyRequest, callback: proto.EntroQ.ModifyCallback): void;

        /**
         * Calls Modify.
         * @param request ModifyRequest message or plain object
         * @returns Promise
         */
        public modify(request: proto.IModifyRequest): Promise<proto.ModifyResponse>;

        /**
         * Calls Tasks.
         * @param request TasksRequest message or plain object
         * @param callback Node-style callback called with the error, if any, and TasksResponse
         */
        public tasks(request: proto.ITasksRequest, callback: proto.EntroQ.TasksCallback): void;

        /**
         * Calls Tasks.
         * @param request TasksRequest message or plain object
         * @returns Promise
         */
        public tasks(request: proto.ITasksRequest): Promise<proto.TasksResponse>;

        /**
         * Calls Queues.
         * @param request QueuesRequest message or plain object
         * @param callback Node-style callback called with the error, if any, and QueuesResponse
         */
        public queues(request: proto.IQueuesRequest, callback: proto.EntroQ.QueuesCallback): void;

        /**
         * Calls Queues.
         * @param request QueuesRequest message or plain object
         * @returns Promise
         */
        public queues(request: proto.IQueuesRequest): Promise<proto.QueuesResponse>;

        /**
         * Calls QueueStats.
         * @param request QueuesRequest message or plain object
         * @param callback Node-style callback called with the error, if any, and QueuesResponse
         */
        public queueStats(request: proto.IQueuesRequest, callback: proto.EntroQ.QueueStatsCallback): void;

        /**
         * Calls QueueStats.
         * @param request QueuesRequest message or plain object
         * @returns Promise
         */
        public queueStats(request: proto.IQueuesRequest): Promise<proto.QueuesResponse>;

        /**
         * Calls Time.
         * @param request TimeRequest message or plain object
         * @param callback Node-style callback called with the error, if any, and TimeResponse
         */
        public time(request: proto.ITimeRequest, callback: proto.EntroQ.TimeCallback): void;

        /**
         * Calls Time.
         * @param request TimeRequest message or plain object
         * @returns Promise
         */
        public time(request: proto.ITimeRequest): Promise<proto.TimeResponse>;

        /**
         * Calls StreamTasks.
         * @param request TasksRequest message or plain object
         * @param callback Node-style callback called with the error, if any, and TasksResponse
         */
        public streamTasks(request: proto.ITasksRequest, callback: proto.EntroQ.StreamTasksCallback): void;

        /**
         * Calls StreamTasks.
         * @param request TasksRequest message or plain object
         * @returns Promise
         */
        public streamTasks(request: proto.ITasksRequest): Promise<proto.TasksResponse>;
    }

    namespace EntroQ {

        /**
         * Callback as used by {@link proto.EntroQ#tryClaim}.
         * @param error Error, if any
         * @param [response] ClaimResponse
         */
        type TryClaimCallback = (error: (Error|null), response?: proto.ClaimResponse) => void;

        /**
         * Callback as used by {@link proto.EntroQ#claim}.
         * @param error Error, if any
         * @param [response] ClaimResponse
         */
        type ClaimCallback = (error: (Error|null), response?: proto.ClaimResponse) => void;

        /**
         * Callback as used by {@link proto.EntroQ#modify}.
         * @param error Error, if any
         * @param [response] ModifyResponse
         */
        type ModifyCallback = (error: (Error|null), response?: proto.ModifyResponse) => void;

        /**
         * Callback as used by {@link proto.EntroQ#tasks}.
         * @param error Error, if any
         * @param [response] TasksResponse
         */
        type TasksCallback = (error: (Error|null), response?: proto.TasksResponse) => void;

        /**
         * Callback as used by {@link proto.EntroQ#queues}.
         * @param error Error, if any
         * @param [response] QueuesResponse
         */
        type QueuesCallback = (error: (Error|null), response?: proto.QueuesResponse) => void;

        /**
         * Callback as used by {@link proto.EntroQ#queueStats}.
         * @param error Error, if any
         * @param [response] QueuesResponse
         */
        type QueueStatsCallback = (error: (Error|null), response?: proto.QueuesResponse) => void;

        /**
         * Callback as used by {@link proto.EntroQ#time}.
         * @param error Error, if any
         * @param [response] TimeResponse
         */
        type TimeCallback = (error: (Error|null), response?: proto.TimeResponse) => void;

        /**
         * Callback as used by {@link proto.EntroQ#streamTasks}.
         * @param error Error, if any
         * @param [response] TasksResponse
         */
        type StreamTasksCallback = (error: (Error|null), response?: proto.TasksResponse) => void;
    }
}
