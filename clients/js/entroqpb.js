/*eslint-disable block-scoped-var, id-length, no-control-regex, no-magic-numbers, no-prototype-builtins, no-redeclare, no-shadow, no-var, sort-vars*/
"use strict";

var $protobuf = require("protobufjs/minimal");

// Common aliases
var $Reader = $protobuf.Reader, $Writer = $protobuf.Writer, $util = $protobuf.util;

// Exported root namespace
var $root = $protobuf.roots["default"] || ($protobuf.roots["default"] = {});

$root.proto = (function() {

    /**
     * Namespace proto.
     * @exports proto
     * @namespace
     */
    var proto = {};

    proto.TaskID = (function() {

        /**
         * Properties of a TaskID.
         * @memberof proto
         * @interface ITaskID
         * @property {string|null} [id] TaskID id
         * @property {number|null} [version] TaskID version
         * @property {string|null} [queue] TaskID queue
         */

        /**
         * Constructs a new TaskID.
         * @memberof proto
         * @classdesc Represents a TaskID.
         * @implements ITaskID
         * @constructor
         * @param {proto.ITaskID=} [properties] Properties to set
         */
        function TaskID(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * TaskID id.
         * @member {string} id
         * @memberof proto.TaskID
         * @instance
         */
        TaskID.prototype.id = "";

        /**
         * TaskID version.
         * @member {number} version
         * @memberof proto.TaskID
         * @instance
         */
        TaskID.prototype.version = 0;

        /**
         * TaskID queue.
         * @member {string} queue
         * @memberof proto.TaskID
         * @instance
         */
        TaskID.prototype.queue = "";

        /**
         * Creates a new TaskID instance using the specified properties.
         * @function create
         * @memberof proto.TaskID
         * @static
         * @param {proto.ITaskID=} [properties] Properties to set
         * @returns {proto.TaskID} TaskID instance
         */
        TaskID.create = function create(properties) {
            return new TaskID(properties);
        };

        /**
         * Encodes the specified TaskID message. Does not implicitly {@link proto.TaskID.verify|verify} messages.
         * @function encode
         * @memberof proto.TaskID
         * @static
         * @param {proto.ITaskID} message TaskID message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TaskID.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.id != null && Object.hasOwnProperty.call(message, "id"))
                writer.uint32(/* id 1, wireType 2 =*/10).string(message.id);
            if (message.version != null && Object.hasOwnProperty.call(message, "version"))
                writer.uint32(/* id 2, wireType 0 =*/16).int32(message.version);
            if (message.queue != null && Object.hasOwnProperty.call(message, "queue"))
                writer.uint32(/* id 3, wireType 2 =*/26).string(message.queue);
            return writer;
        };

        /**
         * Encodes the specified TaskID message, length delimited. Does not implicitly {@link proto.TaskID.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.TaskID
         * @static
         * @param {proto.ITaskID} message TaskID message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TaskID.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a TaskID message from the specified reader or buffer.
         * @function decode
         * @memberof proto.TaskID
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.TaskID} TaskID
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TaskID.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.TaskID();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.id = reader.string();
                    break;
                case 2:
                    message.version = reader.int32();
                    break;
                case 3:
                    message.queue = reader.string();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a TaskID message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.TaskID
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.TaskID} TaskID
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TaskID.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a TaskID message.
         * @function verify
         * @memberof proto.TaskID
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        TaskID.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.id != null && message.hasOwnProperty("id"))
                if (!$util.isString(message.id))
                    return "id: string expected";
            if (message.version != null && message.hasOwnProperty("version"))
                if (!$util.isInteger(message.version))
                    return "version: integer expected";
            if (message.queue != null && message.hasOwnProperty("queue"))
                if (!$util.isString(message.queue))
                    return "queue: string expected";
            return null;
        };

        /**
         * Creates a TaskID message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.TaskID
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.TaskID} TaskID
         */
        TaskID.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.TaskID)
                return object;
            var message = new $root.proto.TaskID();
            if (object.id != null)
                message.id = String(object.id);
            if (object.version != null)
                message.version = object.version | 0;
            if (object.queue != null)
                message.queue = String(object.queue);
            return message;
        };

        /**
         * Creates a plain object from a TaskID message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.TaskID
         * @static
         * @param {proto.TaskID} message TaskID
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        TaskID.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.defaults) {
                object.id = "";
                object.version = 0;
                object.queue = "";
            }
            if (message.id != null && message.hasOwnProperty("id"))
                object.id = message.id;
            if (message.version != null && message.hasOwnProperty("version"))
                object.version = message.version;
            if (message.queue != null && message.hasOwnProperty("queue"))
                object.queue = message.queue;
            return object;
        };

        /**
         * Converts this TaskID to JSON.
         * @function toJSON
         * @memberof proto.TaskID
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        TaskID.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return TaskID;
    })();

    proto.TaskData = (function() {

        /**
         * Properties of a TaskData.
         * @memberof proto
         * @interface ITaskData
         * @property {string|null} [queue] TaskData queue
         * @property {number|Long|null} [atMs] TaskData atMs
         * @property {Uint8Array|null} [value] TaskData value
         * @property {string|null} [id] TaskData id
         * @property {number|null} [attempt] TaskData attempt
         * @property {string|null} [err] TaskData err
         */

        /**
         * Constructs a new TaskData.
         * @memberof proto
         * @classdesc Represents a TaskData.
         * @implements ITaskData
         * @constructor
         * @param {proto.ITaskData=} [properties] Properties to set
         */
        function TaskData(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * TaskData queue.
         * @member {string} queue
         * @memberof proto.TaskData
         * @instance
         */
        TaskData.prototype.queue = "";

        /**
         * TaskData atMs.
         * @member {number|Long} atMs
         * @memberof proto.TaskData
         * @instance
         */
        TaskData.prototype.atMs = $util.Long ? $util.Long.fromBits(0,0,false) : 0;

        /**
         * TaskData value.
         * @member {Uint8Array} value
         * @memberof proto.TaskData
         * @instance
         */
        TaskData.prototype.value = $util.newBuffer([]);

        /**
         * TaskData id.
         * @member {string} id
         * @memberof proto.TaskData
         * @instance
         */
        TaskData.prototype.id = "";

        /**
         * TaskData attempt.
         * @member {number} attempt
         * @memberof proto.TaskData
         * @instance
         */
        TaskData.prototype.attempt = 0;

        /**
         * TaskData err.
         * @member {string} err
         * @memberof proto.TaskData
         * @instance
         */
        TaskData.prototype.err = "";

        /**
         * Creates a new TaskData instance using the specified properties.
         * @function create
         * @memberof proto.TaskData
         * @static
         * @param {proto.ITaskData=} [properties] Properties to set
         * @returns {proto.TaskData} TaskData instance
         */
        TaskData.create = function create(properties) {
            return new TaskData(properties);
        };

        /**
         * Encodes the specified TaskData message. Does not implicitly {@link proto.TaskData.verify|verify} messages.
         * @function encode
         * @memberof proto.TaskData
         * @static
         * @param {proto.ITaskData} message TaskData message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TaskData.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.queue != null && Object.hasOwnProperty.call(message, "queue"))
                writer.uint32(/* id 1, wireType 2 =*/10).string(message.queue);
            if (message.atMs != null && Object.hasOwnProperty.call(message, "atMs"))
                writer.uint32(/* id 2, wireType 0 =*/16).int64(message.atMs);
            if (message.value != null && Object.hasOwnProperty.call(message, "value"))
                writer.uint32(/* id 3, wireType 2 =*/26).bytes(message.value);
            if (message.id != null && Object.hasOwnProperty.call(message, "id"))
                writer.uint32(/* id 4, wireType 2 =*/34).string(message.id);
            if (message.attempt != null && Object.hasOwnProperty.call(message, "attempt"))
                writer.uint32(/* id 5, wireType 0 =*/40).int32(message.attempt);
            if (message.err != null && Object.hasOwnProperty.call(message, "err"))
                writer.uint32(/* id 6, wireType 2 =*/50).string(message.err);
            return writer;
        };

        /**
         * Encodes the specified TaskData message, length delimited. Does not implicitly {@link proto.TaskData.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.TaskData
         * @static
         * @param {proto.ITaskData} message TaskData message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TaskData.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a TaskData message from the specified reader or buffer.
         * @function decode
         * @memberof proto.TaskData
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.TaskData} TaskData
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TaskData.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.TaskData();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.queue = reader.string();
                    break;
                case 2:
                    message.atMs = reader.int64();
                    break;
                case 3:
                    message.value = reader.bytes();
                    break;
                case 4:
                    message.id = reader.string();
                    break;
                case 5:
                    message.attempt = reader.int32();
                    break;
                case 6:
                    message.err = reader.string();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a TaskData message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.TaskData
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.TaskData} TaskData
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TaskData.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a TaskData message.
         * @function verify
         * @memberof proto.TaskData
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        TaskData.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.queue != null && message.hasOwnProperty("queue"))
                if (!$util.isString(message.queue))
                    return "queue: string expected";
            if (message.atMs != null && message.hasOwnProperty("atMs"))
                if (!$util.isInteger(message.atMs) && !(message.atMs && $util.isInteger(message.atMs.low) && $util.isInteger(message.atMs.high)))
                    return "atMs: integer|Long expected";
            if (message.value != null && message.hasOwnProperty("value"))
                if (!(message.value && typeof message.value.length === "number" || $util.isString(message.value)))
                    return "value: buffer expected";
            if (message.id != null && message.hasOwnProperty("id"))
                if (!$util.isString(message.id))
                    return "id: string expected";
            if (message.attempt != null && message.hasOwnProperty("attempt"))
                if (!$util.isInteger(message.attempt))
                    return "attempt: integer expected";
            if (message.err != null && message.hasOwnProperty("err"))
                if (!$util.isString(message.err))
                    return "err: string expected";
            return null;
        };

        /**
         * Creates a TaskData message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.TaskData
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.TaskData} TaskData
         */
        TaskData.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.TaskData)
                return object;
            var message = new $root.proto.TaskData();
            if (object.queue != null)
                message.queue = String(object.queue);
            if (object.atMs != null)
                if ($util.Long)
                    (message.atMs = $util.Long.fromValue(object.atMs)).unsigned = false;
                else if (typeof object.atMs === "string")
                    message.atMs = parseInt(object.atMs, 10);
                else if (typeof object.atMs === "number")
                    message.atMs = object.atMs;
                else if (typeof object.atMs === "object")
                    message.atMs = new $util.LongBits(object.atMs.low >>> 0, object.atMs.high >>> 0).toNumber();
            if (object.value != null)
                if (typeof object.value === "string")
                    $util.base64.decode(object.value, message.value = $util.newBuffer($util.base64.length(object.value)), 0);
                else if (object.value.length)
                    message.value = object.value;
            if (object.id != null)
                message.id = String(object.id);
            if (object.attempt != null)
                message.attempt = object.attempt | 0;
            if (object.err != null)
                message.err = String(object.err);
            return message;
        };

        /**
         * Creates a plain object from a TaskData message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.TaskData
         * @static
         * @param {proto.TaskData} message TaskData
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        TaskData.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.defaults) {
                object.queue = "";
                if ($util.Long) {
                    var long = new $util.Long(0, 0, false);
                    object.atMs = options.longs === String ? long.toString() : options.longs === Number ? long.toNumber() : long;
                } else
                    object.atMs = options.longs === String ? "0" : 0;
                if (options.bytes === String)
                    object.value = "";
                else {
                    object.value = [];
                    if (options.bytes !== Array)
                        object.value = $util.newBuffer(object.value);
                }
                object.id = "";
                object.attempt = 0;
                object.err = "";
            }
            if (message.queue != null && message.hasOwnProperty("queue"))
                object.queue = message.queue;
            if (message.atMs != null && message.hasOwnProperty("atMs"))
                if (typeof message.atMs === "number")
                    object.atMs = options.longs === String ? String(message.atMs) : message.atMs;
                else
                    object.atMs = options.longs === String ? $util.Long.prototype.toString.call(message.atMs) : options.longs === Number ? new $util.LongBits(message.atMs.low >>> 0, message.atMs.high >>> 0).toNumber() : message.atMs;
            if (message.value != null && message.hasOwnProperty("value"))
                object.value = options.bytes === String ? $util.base64.encode(message.value, 0, message.value.length) : options.bytes === Array ? Array.prototype.slice.call(message.value) : message.value;
            if (message.id != null && message.hasOwnProperty("id"))
                object.id = message.id;
            if (message.attempt != null && message.hasOwnProperty("attempt"))
                object.attempt = message.attempt;
            if (message.err != null && message.hasOwnProperty("err"))
                object.err = message.err;
            return object;
        };

        /**
         * Converts this TaskData to JSON.
         * @function toJSON
         * @memberof proto.TaskData
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        TaskData.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return TaskData;
    })();

    proto.TaskChange = (function() {

        /**
         * Properties of a TaskChange.
         * @memberof proto
         * @interface ITaskChange
         * @property {proto.ITaskID|null} [oldId] TaskChange oldId
         * @property {proto.ITaskData|null} [newData] TaskChange newData
         */

        /**
         * Constructs a new TaskChange.
         * @memberof proto
         * @classdesc Represents a TaskChange.
         * @implements ITaskChange
         * @constructor
         * @param {proto.ITaskChange=} [properties] Properties to set
         */
        function TaskChange(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * TaskChange oldId.
         * @member {proto.ITaskID|null|undefined} oldId
         * @memberof proto.TaskChange
         * @instance
         */
        TaskChange.prototype.oldId = null;

        /**
         * TaskChange newData.
         * @member {proto.ITaskData|null|undefined} newData
         * @memberof proto.TaskChange
         * @instance
         */
        TaskChange.prototype.newData = null;

        /**
         * Creates a new TaskChange instance using the specified properties.
         * @function create
         * @memberof proto.TaskChange
         * @static
         * @param {proto.ITaskChange=} [properties] Properties to set
         * @returns {proto.TaskChange} TaskChange instance
         */
        TaskChange.create = function create(properties) {
            return new TaskChange(properties);
        };

        /**
         * Encodes the specified TaskChange message. Does not implicitly {@link proto.TaskChange.verify|verify} messages.
         * @function encode
         * @memberof proto.TaskChange
         * @static
         * @param {proto.ITaskChange} message TaskChange message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TaskChange.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.oldId != null && Object.hasOwnProperty.call(message, "oldId"))
                $root.proto.TaskID.encode(message.oldId, writer.uint32(/* id 1, wireType 2 =*/10).fork()).ldelim();
            if (message.newData != null && Object.hasOwnProperty.call(message, "newData"))
                $root.proto.TaskData.encode(message.newData, writer.uint32(/* id 2, wireType 2 =*/18).fork()).ldelim();
            return writer;
        };

        /**
         * Encodes the specified TaskChange message, length delimited. Does not implicitly {@link proto.TaskChange.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.TaskChange
         * @static
         * @param {proto.ITaskChange} message TaskChange message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TaskChange.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a TaskChange message from the specified reader or buffer.
         * @function decode
         * @memberof proto.TaskChange
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.TaskChange} TaskChange
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TaskChange.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.TaskChange();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.oldId = $root.proto.TaskID.decode(reader, reader.uint32());
                    break;
                case 2:
                    message.newData = $root.proto.TaskData.decode(reader, reader.uint32());
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a TaskChange message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.TaskChange
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.TaskChange} TaskChange
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TaskChange.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a TaskChange message.
         * @function verify
         * @memberof proto.TaskChange
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        TaskChange.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.oldId != null && message.hasOwnProperty("oldId")) {
                var error = $root.proto.TaskID.verify(message.oldId);
                if (error)
                    return "oldId." + error;
            }
            if (message.newData != null && message.hasOwnProperty("newData")) {
                var error = $root.proto.TaskData.verify(message.newData);
                if (error)
                    return "newData." + error;
            }
            return null;
        };

        /**
         * Creates a TaskChange message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.TaskChange
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.TaskChange} TaskChange
         */
        TaskChange.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.TaskChange)
                return object;
            var message = new $root.proto.TaskChange();
            if (object.oldId != null) {
                if (typeof object.oldId !== "object")
                    throw TypeError(".proto.TaskChange.oldId: object expected");
                message.oldId = $root.proto.TaskID.fromObject(object.oldId);
            }
            if (object.newData != null) {
                if (typeof object.newData !== "object")
                    throw TypeError(".proto.TaskChange.newData: object expected");
                message.newData = $root.proto.TaskData.fromObject(object.newData);
            }
            return message;
        };

        /**
         * Creates a plain object from a TaskChange message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.TaskChange
         * @static
         * @param {proto.TaskChange} message TaskChange
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        TaskChange.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.defaults) {
                object.oldId = null;
                object.newData = null;
            }
            if (message.oldId != null && message.hasOwnProperty("oldId"))
                object.oldId = $root.proto.TaskID.toObject(message.oldId, options);
            if (message.newData != null && message.hasOwnProperty("newData"))
                object.newData = $root.proto.TaskData.toObject(message.newData, options);
            return object;
        };

        /**
         * Converts this TaskChange to JSON.
         * @function toJSON
         * @memberof proto.TaskChange
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        TaskChange.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return TaskChange;
    })();

    proto.Task = (function() {

        /**
         * Properties of a Task.
         * @memberof proto
         * @interface ITask
         * @property {string|null} [queue] Task queue
         * @property {string|null} [id] Task id
         * @property {number|null} [version] Task version
         * @property {number|Long|null} [atMs] Task atMs
         * @property {string|null} [claimantId] Task claimantId
         * @property {Uint8Array|null} [value] Task value
         * @property {number|Long|null} [createdMs] Task createdMs
         * @property {number|Long|null} [modifiedMs] Task modifiedMs
         * @property {number|null} [claims] Task claims
         * @property {number|null} [attempt] Task attempt
         * @property {string|null} [err] Task err
         */

        /**
         * Constructs a new Task.
         * @memberof proto
         * @classdesc Represents a Task.
         * @implements ITask
         * @constructor
         * @param {proto.ITask=} [properties] Properties to set
         */
        function Task(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * Task queue.
         * @member {string} queue
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.queue = "";

        /**
         * Task id.
         * @member {string} id
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.id = "";

        /**
         * Task version.
         * @member {number} version
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.version = 0;

        /**
         * Task atMs.
         * @member {number|Long} atMs
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.atMs = $util.Long ? $util.Long.fromBits(0,0,false) : 0;

        /**
         * Task claimantId.
         * @member {string} claimantId
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.claimantId = "";

        /**
         * Task value.
         * @member {Uint8Array} value
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.value = $util.newBuffer([]);

        /**
         * Task createdMs.
         * @member {number|Long} createdMs
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.createdMs = $util.Long ? $util.Long.fromBits(0,0,false) : 0;

        /**
         * Task modifiedMs.
         * @member {number|Long} modifiedMs
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.modifiedMs = $util.Long ? $util.Long.fromBits(0,0,false) : 0;

        /**
         * Task claims.
         * @member {number} claims
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.claims = 0;

        /**
         * Task attempt.
         * @member {number} attempt
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.attempt = 0;

        /**
         * Task err.
         * @member {string} err
         * @memberof proto.Task
         * @instance
         */
        Task.prototype.err = "";

        /**
         * Creates a new Task instance using the specified properties.
         * @function create
         * @memberof proto.Task
         * @static
         * @param {proto.ITask=} [properties] Properties to set
         * @returns {proto.Task} Task instance
         */
        Task.create = function create(properties) {
            return new Task(properties);
        };

        /**
         * Encodes the specified Task message. Does not implicitly {@link proto.Task.verify|verify} messages.
         * @function encode
         * @memberof proto.Task
         * @static
         * @param {proto.ITask} message Task message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        Task.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.queue != null && Object.hasOwnProperty.call(message, "queue"))
                writer.uint32(/* id 1, wireType 2 =*/10).string(message.queue);
            if (message.id != null && Object.hasOwnProperty.call(message, "id"))
                writer.uint32(/* id 2, wireType 2 =*/18).string(message.id);
            if (message.version != null && Object.hasOwnProperty.call(message, "version"))
                writer.uint32(/* id 3, wireType 0 =*/24).int32(message.version);
            if (message.atMs != null && Object.hasOwnProperty.call(message, "atMs"))
                writer.uint32(/* id 4, wireType 0 =*/32).int64(message.atMs);
            if (message.claimantId != null && Object.hasOwnProperty.call(message, "claimantId"))
                writer.uint32(/* id 5, wireType 2 =*/42).string(message.claimantId);
            if (message.value != null && Object.hasOwnProperty.call(message, "value"))
                writer.uint32(/* id 6, wireType 2 =*/50).bytes(message.value);
            if (message.createdMs != null && Object.hasOwnProperty.call(message, "createdMs"))
                writer.uint32(/* id 7, wireType 0 =*/56).int64(message.createdMs);
            if (message.modifiedMs != null && Object.hasOwnProperty.call(message, "modifiedMs"))
                writer.uint32(/* id 8, wireType 0 =*/64).int64(message.modifiedMs);
            if (message.claims != null && Object.hasOwnProperty.call(message, "claims"))
                writer.uint32(/* id 9, wireType 0 =*/72).int32(message.claims);
            if (message.attempt != null && Object.hasOwnProperty.call(message, "attempt"))
                writer.uint32(/* id 10, wireType 0 =*/80).int32(message.attempt);
            if (message.err != null && Object.hasOwnProperty.call(message, "err"))
                writer.uint32(/* id 11, wireType 2 =*/90).string(message.err);
            return writer;
        };

        /**
         * Encodes the specified Task message, length delimited. Does not implicitly {@link proto.Task.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.Task
         * @static
         * @param {proto.ITask} message Task message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        Task.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a Task message from the specified reader or buffer.
         * @function decode
         * @memberof proto.Task
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.Task} Task
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        Task.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.Task();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.queue = reader.string();
                    break;
                case 2:
                    message.id = reader.string();
                    break;
                case 3:
                    message.version = reader.int32();
                    break;
                case 4:
                    message.atMs = reader.int64();
                    break;
                case 5:
                    message.claimantId = reader.string();
                    break;
                case 6:
                    message.value = reader.bytes();
                    break;
                case 7:
                    message.createdMs = reader.int64();
                    break;
                case 8:
                    message.modifiedMs = reader.int64();
                    break;
                case 9:
                    message.claims = reader.int32();
                    break;
                case 10:
                    message.attempt = reader.int32();
                    break;
                case 11:
                    message.err = reader.string();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a Task message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.Task
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.Task} Task
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        Task.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a Task message.
         * @function verify
         * @memberof proto.Task
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        Task.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.queue != null && message.hasOwnProperty("queue"))
                if (!$util.isString(message.queue))
                    return "queue: string expected";
            if (message.id != null && message.hasOwnProperty("id"))
                if (!$util.isString(message.id))
                    return "id: string expected";
            if (message.version != null && message.hasOwnProperty("version"))
                if (!$util.isInteger(message.version))
                    return "version: integer expected";
            if (message.atMs != null && message.hasOwnProperty("atMs"))
                if (!$util.isInteger(message.atMs) && !(message.atMs && $util.isInteger(message.atMs.low) && $util.isInteger(message.atMs.high)))
                    return "atMs: integer|Long expected";
            if (message.claimantId != null && message.hasOwnProperty("claimantId"))
                if (!$util.isString(message.claimantId))
                    return "claimantId: string expected";
            if (message.value != null && message.hasOwnProperty("value"))
                if (!(message.value && typeof message.value.length === "number" || $util.isString(message.value)))
                    return "value: buffer expected";
            if (message.createdMs != null && message.hasOwnProperty("createdMs"))
                if (!$util.isInteger(message.createdMs) && !(message.createdMs && $util.isInteger(message.createdMs.low) && $util.isInteger(message.createdMs.high)))
                    return "createdMs: integer|Long expected";
            if (message.modifiedMs != null && message.hasOwnProperty("modifiedMs"))
                if (!$util.isInteger(message.modifiedMs) && !(message.modifiedMs && $util.isInteger(message.modifiedMs.low) && $util.isInteger(message.modifiedMs.high)))
                    return "modifiedMs: integer|Long expected";
            if (message.claims != null && message.hasOwnProperty("claims"))
                if (!$util.isInteger(message.claims))
                    return "claims: integer expected";
            if (message.attempt != null && message.hasOwnProperty("attempt"))
                if (!$util.isInteger(message.attempt))
                    return "attempt: integer expected";
            if (message.err != null && message.hasOwnProperty("err"))
                if (!$util.isString(message.err))
                    return "err: string expected";
            return null;
        };

        /**
         * Creates a Task message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.Task
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.Task} Task
         */
        Task.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.Task)
                return object;
            var message = new $root.proto.Task();
            if (object.queue != null)
                message.queue = String(object.queue);
            if (object.id != null)
                message.id = String(object.id);
            if (object.version != null)
                message.version = object.version | 0;
            if (object.atMs != null)
                if ($util.Long)
                    (message.atMs = $util.Long.fromValue(object.atMs)).unsigned = false;
                else if (typeof object.atMs === "string")
                    message.atMs = parseInt(object.atMs, 10);
                else if (typeof object.atMs === "number")
                    message.atMs = object.atMs;
                else if (typeof object.atMs === "object")
                    message.atMs = new $util.LongBits(object.atMs.low >>> 0, object.atMs.high >>> 0).toNumber();
            if (object.claimantId != null)
                message.claimantId = String(object.claimantId);
            if (object.value != null)
                if (typeof object.value === "string")
                    $util.base64.decode(object.value, message.value = $util.newBuffer($util.base64.length(object.value)), 0);
                else if (object.value.length)
                    message.value = object.value;
            if (object.createdMs != null)
                if ($util.Long)
                    (message.createdMs = $util.Long.fromValue(object.createdMs)).unsigned = false;
                else if (typeof object.createdMs === "string")
                    message.createdMs = parseInt(object.createdMs, 10);
                else if (typeof object.createdMs === "number")
                    message.createdMs = object.createdMs;
                else if (typeof object.createdMs === "object")
                    message.createdMs = new $util.LongBits(object.createdMs.low >>> 0, object.createdMs.high >>> 0).toNumber();
            if (object.modifiedMs != null)
                if ($util.Long)
                    (message.modifiedMs = $util.Long.fromValue(object.modifiedMs)).unsigned = false;
                else if (typeof object.modifiedMs === "string")
                    message.modifiedMs = parseInt(object.modifiedMs, 10);
                else if (typeof object.modifiedMs === "number")
                    message.modifiedMs = object.modifiedMs;
                else if (typeof object.modifiedMs === "object")
                    message.modifiedMs = new $util.LongBits(object.modifiedMs.low >>> 0, object.modifiedMs.high >>> 0).toNumber();
            if (object.claims != null)
                message.claims = object.claims | 0;
            if (object.attempt != null)
                message.attempt = object.attempt | 0;
            if (object.err != null)
                message.err = String(object.err);
            return message;
        };

        /**
         * Creates a plain object from a Task message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.Task
         * @static
         * @param {proto.Task} message Task
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        Task.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.defaults) {
                object.queue = "";
                object.id = "";
                object.version = 0;
                if ($util.Long) {
                    var long = new $util.Long(0, 0, false);
                    object.atMs = options.longs === String ? long.toString() : options.longs === Number ? long.toNumber() : long;
                } else
                    object.atMs = options.longs === String ? "0" : 0;
                object.claimantId = "";
                if (options.bytes === String)
                    object.value = "";
                else {
                    object.value = [];
                    if (options.bytes !== Array)
                        object.value = $util.newBuffer(object.value);
                }
                if ($util.Long) {
                    var long = new $util.Long(0, 0, false);
                    object.createdMs = options.longs === String ? long.toString() : options.longs === Number ? long.toNumber() : long;
                } else
                    object.createdMs = options.longs === String ? "0" : 0;
                if ($util.Long) {
                    var long = new $util.Long(0, 0, false);
                    object.modifiedMs = options.longs === String ? long.toString() : options.longs === Number ? long.toNumber() : long;
                } else
                    object.modifiedMs = options.longs === String ? "0" : 0;
                object.claims = 0;
                object.attempt = 0;
                object.err = "";
            }
            if (message.queue != null && message.hasOwnProperty("queue"))
                object.queue = message.queue;
            if (message.id != null && message.hasOwnProperty("id"))
                object.id = message.id;
            if (message.version != null && message.hasOwnProperty("version"))
                object.version = message.version;
            if (message.atMs != null && message.hasOwnProperty("atMs"))
                if (typeof message.atMs === "number")
                    object.atMs = options.longs === String ? String(message.atMs) : message.atMs;
                else
                    object.atMs = options.longs === String ? $util.Long.prototype.toString.call(message.atMs) : options.longs === Number ? new $util.LongBits(message.atMs.low >>> 0, message.atMs.high >>> 0).toNumber() : message.atMs;
            if (message.claimantId != null && message.hasOwnProperty("claimantId"))
                object.claimantId = message.claimantId;
            if (message.value != null && message.hasOwnProperty("value"))
                object.value = options.bytes === String ? $util.base64.encode(message.value, 0, message.value.length) : options.bytes === Array ? Array.prototype.slice.call(message.value) : message.value;
            if (message.createdMs != null && message.hasOwnProperty("createdMs"))
                if (typeof message.createdMs === "number")
                    object.createdMs = options.longs === String ? String(message.createdMs) : message.createdMs;
                else
                    object.createdMs = options.longs === String ? $util.Long.prototype.toString.call(message.createdMs) : options.longs === Number ? new $util.LongBits(message.createdMs.low >>> 0, message.createdMs.high >>> 0).toNumber() : message.createdMs;
            if (message.modifiedMs != null && message.hasOwnProperty("modifiedMs"))
                if (typeof message.modifiedMs === "number")
                    object.modifiedMs = options.longs === String ? String(message.modifiedMs) : message.modifiedMs;
                else
                    object.modifiedMs = options.longs === String ? $util.Long.prototype.toString.call(message.modifiedMs) : options.longs === Number ? new $util.LongBits(message.modifiedMs.low >>> 0, message.modifiedMs.high >>> 0).toNumber() : message.modifiedMs;
            if (message.claims != null && message.hasOwnProperty("claims"))
                object.claims = message.claims;
            if (message.attempt != null && message.hasOwnProperty("attempt"))
                object.attempt = message.attempt;
            if (message.err != null && message.hasOwnProperty("err"))
                object.err = message.err;
            return object;
        };

        /**
         * Converts this Task to JSON.
         * @function toJSON
         * @memberof proto.Task
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        Task.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return Task;
    })();

    proto.QueueStats = (function() {

        /**
         * Properties of a QueueStats.
         * @memberof proto
         * @interface IQueueStats
         * @property {string|null} [name] QueueStats name
         * @property {number|null} [numTasks] QueueStats numTasks
         * @property {number|null} [numClaimed] QueueStats numClaimed
         * @property {number|null} [numAvailable] QueueStats numAvailable
         * @property {number|null} [maxClaims] QueueStats maxClaims
         */

        /**
         * Constructs a new QueueStats.
         * @memberof proto
         * @classdesc Represents a QueueStats.
         * @implements IQueueStats
         * @constructor
         * @param {proto.IQueueStats=} [properties] Properties to set
         */
        function QueueStats(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * QueueStats name.
         * @member {string} name
         * @memberof proto.QueueStats
         * @instance
         */
        QueueStats.prototype.name = "";

        /**
         * QueueStats numTasks.
         * @member {number} numTasks
         * @memberof proto.QueueStats
         * @instance
         */
        QueueStats.prototype.numTasks = 0;

        /**
         * QueueStats numClaimed.
         * @member {number} numClaimed
         * @memberof proto.QueueStats
         * @instance
         */
        QueueStats.prototype.numClaimed = 0;

        /**
         * QueueStats numAvailable.
         * @member {number} numAvailable
         * @memberof proto.QueueStats
         * @instance
         */
        QueueStats.prototype.numAvailable = 0;

        /**
         * QueueStats maxClaims.
         * @member {number} maxClaims
         * @memberof proto.QueueStats
         * @instance
         */
        QueueStats.prototype.maxClaims = 0;

        /**
         * Creates a new QueueStats instance using the specified properties.
         * @function create
         * @memberof proto.QueueStats
         * @static
         * @param {proto.IQueueStats=} [properties] Properties to set
         * @returns {proto.QueueStats} QueueStats instance
         */
        QueueStats.create = function create(properties) {
            return new QueueStats(properties);
        };

        /**
         * Encodes the specified QueueStats message. Does not implicitly {@link proto.QueueStats.verify|verify} messages.
         * @function encode
         * @memberof proto.QueueStats
         * @static
         * @param {proto.IQueueStats} message QueueStats message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        QueueStats.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.name != null && Object.hasOwnProperty.call(message, "name"))
                writer.uint32(/* id 1, wireType 2 =*/10).string(message.name);
            if (message.numTasks != null && Object.hasOwnProperty.call(message, "numTasks"))
                writer.uint32(/* id 2, wireType 0 =*/16).int32(message.numTasks);
            if (message.numClaimed != null && Object.hasOwnProperty.call(message, "numClaimed"))
                writer.uint32(/* id 3, wireType 0 =*/24).int32(message.numClaimed);
            if (message.numAvailable != null && Object.hasOwnProperty.call(message, "numAvailable"))
                writer.uint32(/* id 4, wireType 0 =*/32).int32(message.numAvailable);
            if (message.maxClaims != null && Object.hasOwnProperty.call(message, "maxClaims"))
                writer.uint32(/* id 5, wireType 0 =*/40).int32(message.maxClaims);
            return writer;
        };

        /**
         * Encodes the specified QueueStats message, length delimited. Does not implicitly {@link proto.QueueStats.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.QueueStats
         * @static
         * @param {proto.IQueueStats} message QueueStats message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        QueueStats.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a QueueStats message from the specified reader or buffer.
         * @function decode
         * @memberof proto.QueueStats
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.QueueStats} QueueStats
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        QueueStats.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.QueueStats();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.name = reader.string();
                    break;
                case 2:
                    message.numTasks = reader.int32();
                    break;
                case 3:
                    message.numClaimed = reader.int32();
                    break;
                case 4:
                    message.numAvailable = reader.int32();
                    break;
                case 5:
                    message.maxClaims = reader.int32();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a QueueStats message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.QueueStats
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.QueueStats} QueueStats
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        QueueStats.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a QueueStats message.
         * @function verify
         * @memberof proto.QueueStats
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        QueueStats.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.name != null && message.hasOwnProperty("name"))
                if (!$util.isString(message.name))
                    return "name: string expected";
            if (message.numTasks != null && message.hasOwnProperty("numTasks"))
                if (!$util.isInteger(message.numTasks))
                    return "numTasks: integer expected";
            if (message.numClaimed != null && message.hasOwnProperty("numClaimed"))
                if (!$util.isInteger(message.numClaimed))
                    return "numClaimed: integer expected";
            if (message.numAvailable != null && message.hasOwnProperty("numAvailable"))
                if (!$util.isInteger(message.numAvailable))
                    return "numAvailable: integer expected";
            if (message.maxClaims != null && message.hasOwnProperty("maxClaims"))
                if (!$util.isInteger(message.maxClaims))
                    return "maxClaims: integer expected";
            return null;
        };

        /**
         * Creates a QueueStats message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.QueueStats
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.QueueStats} QueueStats
         */
        QueueStats.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.QueueStats)
                return object;
            var message = new $root.proto.QueueStats();
            if (object.name != null)
                message.name = String(object.name);
            if (object.numTasks != null)
                message.numTasks = object.numTasks | 0;
            if (object.numClaimed != null)
                message.numClaimed = object.numClaimed | 0;
            if (object.numAvailable != null)
                message.numAvailable = object.numAvailable | 0;
            if (object.maxClaims != null)
                message.maxClaims = object.maxClaims | 0;
            return message;
        };

        /**
         * Creates a plain object from a QueueStats message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.QueueStats
         * @static
         * @param {proto.QueueStats} message QueueStats
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        QueueStats.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.defaults) {
                object.name = "";
                object.numTasks = 0;
                object.numClaimed = 0;
                object.numAvailable = 0;
                object.maxClaims = 0;
            }
            if (message.name != null && message.hasOwnProperty("name"))
                object.name = message.name;
            if (message.numTasks != null && message.hasOwnProperty("numTasks"))
                object.numTasks = message.numTasks;
            if (message.numClaimed != null && message.hasOwnProperty("numClaimed"))
                object.numClaimed = message.numClaimed;
            if (message.numAvailable != null && message.hasOwnProperty("numAvailable"))
                object.numAvailable = message.numAvailable;
            if (message.maxClaims != null && message.hasOwnProperty("maxClaims"))
                object.maxClaims = message.maxClaims;
            return object;
        };

        /**
         * Converts this QueueStats to JSON.
         * @function toJSON
         * @memberof proto.QueueStats
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        QueueStats.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return QueueStats;
    })();

    proto.ClaimRequest = (function() {

        /**
         * Properties of a ClaimRequest.
         * @memberof proto
         * @interface IClaimRequest
         * @property {string|null} [claimantId] ClaimRequest claimantId
         * @property {Array.<string>|null} [queues] ClaimRequest queues
         * @property {number|Long|null} [durationMs] ClaimRequest durationMs
         * @property {number|Long|null} [pollMs] ClaimRequest pollMs
         */

        /**
         * Constructs a new ClaimRequest.
         * @memberof proto
         * @classdesc Represents a ClaimRequest.
         * @implements IClaimRequest
         * @constructor
         * @param {proto.IClaimRequest=} [properties] Properties to set
         */
        function ClaimRequest(properties) {
            this.queues = [];
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * ClaimRequest claimantId.
         * @member {string} claimantId
         * @memberof proto.ClaimRequest
         * @instance
         */
        ClaimRequest.prototype.claimantId = "";

        /**
         * ClaimRequest queues.
         * @member {Array.<string>} queues
         * @memberof proto.ClaimRequest
         * @instance
         */
        ClaimRequest.prototype.queues = $util.emptyArray;

        /**
         * ClaimRequest durationMs.
         * @member {number|Long} durationMs
         * @memberof proto.ClaimRequest
         * @instance
         */
        ClaimRequest.prototype.durationMs = $util.Long ? $util.Long.fromBits(0,0,false) : 0;

        /**
         * ClaimRequest pollMs.
         * @member {number|Long} pollMs
         * @memberof proto.ClaimRequest
         * @instance
         */
        ClaimRequest.prototype.pollMs = $util.Long ? $util.Long.fromBits(0,0,false) : 0;

        /**
         * Creates a new ClaimRequest instance using the specified properties.
         * @function create
         * @memberof proto.ClaimRequest
         * @static
         * @param {proto.IClaimRequest=} [properties] Properties to set
         * @returns {proto.ClaimRequest} ClaimRequest instance
         */
        ClaimRequest.create = function create(properties) {
            return new ClaimRequest(properties);
        };

        /**
         * Encodes the specified ClaimRequest message. Does not implicitly {@link proto.ClaimRequest.verify|verify} messages.
         * @function encode
         * @memberof proto.ClaimRequest
         * @static
         * @param {proto.IClaimRequest} message ClaimRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ClaimRequest.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.claimantId != null && Object.hasOwnProperty.call(message, "claimantId"))
                writer.uint32(/* id 1, wireType 2 =*/10).string(message.claimantId);
            if (message.queues != null && message.queues.length)
                for (var i = 0; i < message.queues.length; ++i)
                    writer.uint32(/* id 2, wireType 2 =*/18).string(message.queues[i]);
            if (message.durationMs != null && Object.hasOwnProperty.call(message, "durationMs"))
                writer.uint32(/* id 3, wireType 0 =*/24).int64(message.durationMs);
            if (message.pollMs != null && Object.hasOwnProperty.call(message, "pollMs"))
                writer.uint32(/* id 4, wireType 0 =*/32).int64(message.pollMs);
            return writer;
        };

        /**
         * Encodes the specified ClaimRequest message, length delimited. Does not implicitly {@link proto.ClaimRequest.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.ClaimRequest
         * @static
         * @param {proto.IClaimRequest} message ClaimRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ClaimRequest.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a ClaimRequest message from the specified reader or buffer.
         * @function decode
         * @memberof proto.ClaimRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.ClaimRequest} ClaimRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ClaimRequest.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.ClaimRequest();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.claimantId = reader.string();
                    break;
                case 2:
                    if (!(message.queues && message.queues.length))
                        message.queues = [];
                    message.queues.push(reader.string());
                    break;
                case 3:
                    message.durationMs = reader.int64();
                    break;
                case 4:
                    message.pollMs = reader.int64();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a ClaimRequest message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.ClaimRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.ClaimRequest} ClaimRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ClaimRequest.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a ClaimRequest message.
         * @function verify
         * @memberof proto.ClaimRequest
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        ClaimRequest.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.claimantId != null && message.hasOwnProperty("claimantId"))
                if (!$util.isString(message.claimantId))
                    return "claimantId: string expected";
            if (message.queues != null && message.hasOwnProperty("queues")) {
                if (!Array.isArray(message.queues))
                    return "queues: array expected";
                for (var i = 0; i < message.queues.length; ++i)
                    if (!$util.isString(message.queues[i]))
                        return "queues: string[] expected";
            }
            if (message.durationMs != null && message.hasOwnProperty("durationMs"))
                if (!$util.isInteger(message.durationMs) && !(message.durationMs && $util.isInteger(message.durationMs.low) && $util.isInteger(message.durationMs.high)))
                    return "durationMs: integer|Long expected";
            if (message.pollMs != null && message.hasOwnProperty("pollMs"))
                if (!$util.isInteger(message.pollMs) && !(message.pollMs && $util.isInteger(message.pollMs.low) && $util.isInteger(message.pollMs.high)))
                    return "pollMs: integer|Long expected";
            return null;
        };

        /**
         * Creates a ClaimRequest message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.ClaimRequest
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.ClaimRequest} ClaimRequest
         */
        ClaimRequest.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.ClaimRequest)
                return object;
            var message = new $root.proto.ClaimRequest();
            if (object.claimantId != null)
                message.claimantId = String(object.claimantId);
            if (object.queues) {
                if (!Array.isArray(object.queues))
                    throw TypeError(".proto.ClaimRequest.queues: array expected");
                message.queues = [];
                for (var i = 0; i < object.queues.length; ++i)
                    message.queues[i] = String(object.queues[i]);
            }
            if (object.durationMs != null)
                if ($util.Long)
                    (message.durationMs = $util.Long.fromValue(object.durationMs)).unsigned = false;
                else if (typeof object.durationMs === "string")
                    message.durationMs = parseInt(object.durationMs, 10);
                else if (typeof object.durationMs === "number")
                    message.durationMs = object.durationMs;
                else if (typeof object.durationMs === "object")
                    message.durationMs = new $util.LongBits(object.durationMs.low >>> 0, object.durationMs.high >>> 0).toNumber();
            if (object.pollMs != null)
                if ($util.Long)
                    (message.pollMs = $util.Long.fromValue(object.pollMs)).unsigned = false;
                else if (typeof object.pollMs === "string")
                    message.pollMs = parseInt(object.pollMs, 10);
                else if (typeof object.pollMs === "number")
                    message.pollMs = object.pollMs;
                else if (typeof object.pollMs === "object")
                    message.pollMs = new $util.LongBits(object.pollMs.low >>> 0, object.pollMs.high >>> 0).toNumber();
            return message;
        };

        /**
         * Creates a plain object from a ClaimRequest message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.ClaimRequest
         * @static
         * @param {proto.ClaimRequest} message ClaimRequest
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        ClaimRequest.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.arrays || options.defaults)
                object.queues = [];
            if (options.defaults) {
                object.claimantId = "";
                if ($util.Long) {
                    var long = new $util.Long(0, 0, false);
                    object.durationMs = options.longs === String ? long.toString() : options.longs === Number ? long.toNumber() : long;
                } else
                    object.durationMs = options.longs === String ? "0" : 0;
                if ($util.Long) {
                    var long = new $util.Long(0, 0, false);
                    object.pollMs = options.longs === String ? long.toString() : options.longs === Number ? long.toNumber() : long;
                } else
                    object.pollMs = options.longs === String ? "0" : 0;
            }
            if (message.claimantId != null && message.hasOwnProperty("claimantId"))
                object.claimantId = message.claimantId;
            if (message.queues && message.queues.length) {
                object.queues = [];
                for (var j = 0; j < message.queues.length; ++j)
                    object.queues[j] = message.queues[j];
            }
            if (message.durationMs != null && message.hasOwnProperty("durationMs"))
                if (typeof message.durationMs === "number")
                    object.durationMs = options.longs === String ? String(message.durationMs) : message.durationMs;
                else
                    object.durationMs = options.longs === String ? $util.Long.prototype.toString.call(message.durationMs) : options.longs === Number ? new $util.LongBits(message.durationMs.low >>> 0, message.durationMs.high >>> 0).toNumber() : message.durationMs;
            if (message.pollMs != null && message.hasOwnProperty("pollMs"))
                if (typeof message.pollMs === "number")
                    object.pollMs = options.longs === String ? String(message.pollMs) : message.pollMs;
                else
                    object.pollMs = options.longs === String ? $util.Long.prototype.toString.call(message.pollMs) : options.longs === Number ? new $util.LongBits(message.pollMs.low >>> 0, message.pollMs.high >>> 0).toNumber() : message.pollMs;
            return object;
        };

        /**
         * Converts this ClaimRequest to JSON.
         * @function toJSON
         * @memberof proto.ClaimRequest
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        ClaimRequest.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return ClaimRequest;
    })();

    proto.ClaimResponse = (function() {

        /**
         * Properties of a ClaimResponse.
         * @memberof proto
         * @interface IClaimResponse
         * @property {proto.ITask|null} [task] ClaimResponse task
         */

        /**
         * Constructs a new ClaimResponse.
         * @memberof proto
         * @classdesc Represents a ClaimResponse.
         * @implements IClaimResponse
         * @constructor
         * @param {proto.IClaimResponse=} [properties] Properties to set
         */
        function ClaimResponse(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * ClaimResponse task.
         * @member {proto.ITask|null|undefined} task
         * @memberof proto.ClaimResponse
         * @instance
         */
        ClaimResponse.prototype.task = null;

        /**
         * Creates a new ClaimResponse instance using the specified properties.
         * @function create
         * @memberof proto.ClaimResponse
         * @static
         * @param {proto.IClaimResponse=} [properties] Properties to set
         * @returns {proto.ClaimResponse} ClaimResponse instance
         */
        ClaimResponse.create = function create(properties) {
            return new ClaimResponse(properties);
        };

        /**
         * Encodes the specified ClaimResponse message. Does not implicitly {@link proto.ClaimResponse.verify|verify} messages.
         * @function encode
         * @memberof proto.ClaimResponse
         * @static
         * @param {proto.IClaimResponse} message ClaimResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ClaimResponse.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.task != null && Object.hasOwnProperty.call(message, "task"))
                $root.proto.Task.encode(message.task, writer.uint32(/* id 1, wireType 2 =*/10).fork()).ldelim();
            return writer;
        };

        /**
         * Encodes the specified ClaimResponse message, length delimited. Does not implicitly {@link proto.ClaimResponse.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.ClaimResponse
         * @static
         * @param {proto.IClaimResponse} message ClaimResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ClaimResponse.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a ClaimResponse message from the specified reader or buffer.
         * @function decode
         * @memberof proto.ClaimResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.ClaimResponse} ClaimResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ClaimResponse.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.ClaimResponse();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.task = $root.proto.Task.decode(reader, reader.uint32());
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a ClaimResponse message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.ClaimResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.ClaimResponse} ClaimResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ClaimResponse.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a ClaimResponse message.
         * @function verify
         * @memberof proto.ClaimResponse
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        ClaimResponse.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.task != null && message.hasOwnProperty("task")) {
                var error = $root.proto.Task.verify(message.task);
                if (error)
                    return "task." + error;
            }
            return null;
        };

        /**
         * Creates a ClaimResponse message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.ClaimResponse
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.ClaimResponse} ClaimResponse
         */
        ClaimResponse.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.ClaimResponse)
                return object;
            var message = new $root.proto.ClaimResponse();
            if (object.task != null) {
                if (typeof object.task !== "object")
                    throw TypeError(".proto.ClaimResponse.task: object expected");
                message.task = $root.proto.Task.fromObject(object.task);
            }
            return message;
        };

        /**
         * Creates a plain object from a ClaimResponse message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.ClaimResponse
         * @static
         * @param {proto.ClaimResponse} message ClaimResponse
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        ClaimResponse.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.defaults)
                object.task = null;
            if (message.task != null && message.hasOwnProperty("task"))
                object.task = $root.proto.Task.toObject(message.task, options);
            return object;
        };

        /**
         * Converts this ClaimResponse to JSON.
         * @function toJSON
         * @memberof proto.ClaimResponse
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        ClaimResponse.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return ClaimResponse;
    })();

    proto.ModifyRequest = (function() {

        /**
         * Properties of a ModifyRequest.
         * @memberof proto
         * @interface IModifyRequest
         * @property {string|null} [claimantId] ModifyRequest claimantId
         * @property {Array.<proto.ITaskData>|null} [inserts] ModifyRequest inserts
         * @property {Array.<proto.ITaskChange>|null} [changes] ModifyRequest changes
         * @property {Array.<proto.ITaskID>|null} [deletes] ModifyRequest deletes
         * @property {Array.<proto.ITaskID>|null} [depends] ModifyRequest depends
         */

        /**
         * Constructs a new ModifyRequest.
         * @memberof proto
         * @classdesc Represents a ModifyRequest.
         * @implements IModifyRequest
         * @constructor
         * @param {proto.IModifyRequest=} [properties] Properties to set
         */
        function ModifyRequest(properties) {
            this.inserts = [];
            this.changes = [];
            this.deletes = [];
            this.depends = [];
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * ModifyRequest claimantId.
         * @member {string} claimantId
         * @memberof proto.ModifyRequest
         * @instance
         */
        ModifyRequest.prototype.claimantId = "";

        /**
         * ModifyRequest inserts.
         * @member {Array.<proto.ITaskData>} inserts
         * @memberof proto.ModifyRequest
         * @instance
         */
        ModifyRequest.prototype.inserts = $util.emptyArray;

        /**
         * ModifyRequest changes.
         * @member {Array.<proto.ITaskChange>} changes
         * @memberof proto.ModifyRequest
         * @instance
         */
        ModifyRequest.prototype.changes = $util.emptyArray;

        /**
         * ModifyRequest deletes.
         * @member {Array.<proto.ITaskID>} deletes
         * @memberof proto.ModifyRequest
         * @instance
         */
        ModifyRequest.prototype.deletes = $util.emptyArray;

        /**
         * ModifyRequest depends.
         * @member {Array.<proto.ITaskID>} depends
         * @memberof proto.ModifyRequest
         * @instance
         */
        ModifyRequest.prototype.depends = $util.emptyArray;

        /**
         * Creates a new ModifyRequest instance using the specified properties.
         * @function create
         * @memberof proto.ModifyRequest
         * @static
         * @param {proto.IModifyRequest=} [properties] Properties to set
         * @returns {proto.ModifyRequest} ModifyRequest instance
         */
        ModifyRequest.create = function create(properties) {
            return new ModifyRequest(properties);
        };

        /**
         * Encodes the specified ModifyRequest message. Does not implicitly {@link proto.ModifyRequest.verify|verify} messages.
         * @function encode
         * @memberof proto.ModifyRequest
         * @static
         * @param {proto.IModifyRequest} message ModifyRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ModifyRequest.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.claimantId != null && Object.hasOwnProperty.call(message, "claimantId"))
                writer.uint32(/* id 1, wireType 2 =*/10).string(message.claimantId);
            if (message.inserts != null && message.inserts.length)
                for (var i = 0; i < message.inserts.length; ++i)
                    $root.proto.TaskData.encode(message.inserts[i], writer.uint32(/* id 2, wireType 2 =*/18).fork()).ldelim();
            if (message.changes != null && message.changes.length)
                for (var i = 0; i < message.changes.length; ++i)
                    $root.proto.TaskChange.encode(message.changes[i], writer.uint32(/* id 3, wireType 2 =*/26).fork()).ldelim();
            if (message.deletes != null && message.deletes.length)
                for (var i = 0; i < message.deletes.length; ++i)
                    $root.proto.TaskID.encode(message.deletes[i], writer.uint32(/* id 4, wireType 2 =*/34).fork()).ldelim();
            if (message.depends != null && message.depends.length)
                for (var i = 0; i < message.depends.length; ++i)
                    $root.proto.TaskID.encode(message.depends[i], writer.uint32(/* id 5, wireType 2 =*/42).fork()).ldelim();
            return writer;
        };

        /**
         * Encodes the specified ModifyRequest message, length delimited. Does not implicitly {@link proto.ModifyRequest.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.ModifyRequest
         * @static
         * @param {proto.IModifyRequest} message ModifyRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ModifyRequest.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a ModifyRequest message from the specified reader or buffer.
         * @function decode
         * @memberof proto.ModifyRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.ModifyRequest} ModifyRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ModifyRequest.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.ModifyRequest();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.claimantId = reader.string();
                    break;
                case 2:
                    if (!(message.inserts && message.inserts.length))
                        message.inserts = [];
                    message.inserts.push($root.proto.TaskData.decode(reader, reader.uint32()));
                    break;
                case 3:
                    if (!(message.changes && message.changes.length))
                        message.changes = [];
                    message.changes.push($root.proto.TaskChange.decode(reader, reader.uint32()));
                    break;
                case 4:
                    if (!(message.deletes && message.deletes.length))
                        message.deletes = [];
                    message.deletes.push($root.proto.TaskID.decode(reader, reader.uint32()));
                    break;
                case 5:
                    if (!(message.depends && message.depends.length))
                        message.depends = [];
                    message.depends.push($root.proto.TaskID.decode(reader, reader.uint32()));
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a ModifyRequest message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.ModifyRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.ModifyRequest} ModifyRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ModifyRequest.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a ModifyRequest message.
         * @function verify
         * @memberof proto.ModifyRequest
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        ModifyRequest.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.claimantId != null && message.hasOwnProperty("claimantId"))
                if (!$util.isString(message.claimantId))
                    return "claimantId: string expected";
            if (message.inserts != null && message.hasOwnProperty("inserts")) {
                if (!Array.isArray(message.inserts))
                    return "inserts: array expected";
                for (var i = 0; i < message.inserts.length; ++i) {
                    var error = $root.proto.TaskData.verify(message.inserts[i]);
                    if (error)
                        return "inserts." + error;
                }
            }
            if (message.changes != null && message.hasOwnProperty("changes")) {
                if (!Array.isArray(message.changes))
                    return "changes: array expected";
                for (var i = 0; i < message.changes.length; ++i) {
                    var error = $root.proto.TaskChange.verify(message.changes[i]);
                    if (error)
                        return "changes." + error;
                }
            }
            if (message.deletes != null && message.hasOwnProperty("deletes")) {
                if (!Array.isArray(message.deletes))
                    return "deletes: array expected";
                for (var i = 0; i < message.deletes.length; ++i) {
                    var error = $root.proto.TaskID.verify(message.deletes[i]);
                    if (error)
                        return "deletes." + error;
                }
            }
            if (message.depends != null && message.hasOwnProperty("depends")) {
                if (!Array.isArray(message.depends))
                    return "depends: array expected";
                for (var i = 0; i < message.depends.length; ++i) {
                    var error = $root.proto.TaskID.verify(message.depends[i]);
                    if (error)
                        return "depends." + error;
                }
            }
            return null;
        };

        /**
         * Creates a ModifyRequest message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.ModifyRequest
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.ModifyRequest} ModifyRequest
         */
        ModifyRequest.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.ModifyRequest)
                return object;
            var message = new $root.proto.ModifyRequest();
            if (object.claimantId != null)
                message.claimantId = String(object.claimantId);
            if (object.inserts) {
                if (!Array.isArray(object.inserts))
                    throw TypeError(".proto.ModifyRequest.inserts: array expected");
                message.inserts = [];
                for (var i = 0; i < object.inserts.length; ++i) {
                    if (typeof object.inserts[i] !== "object")
                        throw TypeError(".proto.ModifyRequest.inserts: object expected");
                    message.inserts[i] = $root.proto.TaskData.fromObject(object.inserts[i]);
                }
            }
            if (object.changes) {
                if (!Array.isArray(object.changes))
                    throw TypeError(".proto.ModifyRequest.changes: array expected");
                message.changes = [];
                for (var i = 0; i < object.changes.length; ++i) {
                    if (typeof object.changes[i] !== "object")
                        throw TypeError(".proto.ModifyRequest.changes: object expected");
                    message.changes[i] = $root.proto.TaskChange.fromObject(object.changes[i]);
                }
            }
            if (object.deletes) {
                if (!Array.isArray(object.deletes))
                    throw TypeError(".proto.ModifyRequest.deletes: array expected");
                message.deletes = [];
                for (var i = 0; i < object.deletes.length; ++i) {
                    if (typeof object.deletes[i] !== "object")
                        throw TypeError(".proto.ModifyRequest.deletes: object expected");
                    message.deletes[i] = $root.proto.TaskID.fromObject(object.deletes[i]);
                }
            }
            if (object.depends) {
                if (!Array.isArray(object.depends))
                    throw TypeError(".proto.ModifyRequest.depends: array expected");
                message.depends = [];
                for (var i = 0; i < object.depends.length; ++i) {
                    if (typeof object.depends[i] !== "object")
                        throw TypeError(".proto.ModifyRequest.depends: object expected");
                    message.depends[i] = $root.proto.TaskID.fromObject(object.depends[i]);
                }
            }
            return message;
        };

        /**
         * Creates a plain object from a ModifyRequest message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.ModifyRequest
         * @static
         * @param {proto.ModifyRequest} message ModifyRequest
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        ModifyRequest.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.arrays || options.defaults) {
                object.inserts = [];
                object.changes = [];
                object.deletes = [];
                object.depends = [];
            }
            if (options.defaults)
                object.claimantId = "";
            if (message.claimantId != null && message.hasOwnProperty("claimantId"))
                object.claimantId = message.claimantId;
            if (message.inserts && message.inserts.length) {
                object.inserts = [];
                for (var j = 0; j < message.inserts.length; ++j)
                    object.inserts[j] = $root.proto.TaskData.toObject(message.inserts[j], options);
            }
            if (message.changes && message.changes.length) {
                object.changes = [];
                for (var j = 0; j < message.changes.length; ++j)
                    object.changes[j] = $root.proto.TaskChange.toObject(message.changes[j], options);
            }
            if (message.deletes && message.deletes.length) {
                object.deletes = [];
                for (var j = 0; j < message.deletes.length; ++j)
                    object.deletes[j] = $root.proto.TaskID.toObject(message.deletes[j], options);
            }
            if (message.depends && message.depends.length) {
                object.depends = [];
                for (var j = 0; j < message.depends.length; ++j)
                    object.depends[j] = $root.proto.TaskID.toObject(message.depends[j], options);
            }
            return object;
        };

        /**
         * Converts this ModifyRequest to JSON.
         * @function toJSON
         * @memberof proto.ModifyRequest
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        ModifyRequest.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return ModifyRequest;
    })();

    proto.ModifyResponse = (function() {

        /**
         * Properties of a ModifyResponse.
         * @memberof proto
         * @interface IModifyResponse
         * @property {Array.<proto.ITask>|null} [inserted] ModifyResponse inserted
         * @property {Array.<proto.ITask>|null} [changed] ModifyResponse changed
         */

        /**
         * Constructs a new ModifyResponse.
         * @memberof proto
         * @classdesc Represents a ModifyResponse.
         * @implements IModifyResponse
         * @constructor
         * @param {proto.IModifyResponse=} [properties] Properties to set
         */
        function ModifyResponse(properties) {
            this.inserted = [];
            this.changed = [];
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * ModifyResponse inserted.
         * @member {Array.<proto.ITask>} inserted
         * @memberof proto.ModifyResponse
         * @instance
         */
        ModifyResponse.prototype.inserted = $util.emptyArray;

        /**
         * ModifyResponse changed.
         * @member {Array.<proto.ITask>} changed
         * @memberof proto.ModifyResponse
         * @instance
         */
        ModifyResponse.prototype.changed = $util.emptyArray;

        /**
         * Creates a new ModifyResponse instance using the specified properties.
         * @function create
         * @memberof proto.ModifyResponse
         * @static
         * @param {proto.IModifyResponse=} [properties] Properties to set
         * @returns {proto.ModifyResponse} ModifyResponse instance
         */
        ModifyResponse.create = function create(properties) {
            return new ModifyResponse(properties);
        };

        /**
         * Encodes the specified ModifyResponse message. Does not implicitly {@link proto.ModifyResponse.verify|verify} messages.
         * @function encode
         * @memberof proto.ModifyResponse
         * @static
         * @param {proto.IModifyResponse} message ModifyResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ModifyResponse.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.inserted != null && message.inserted.length)
                for (var i = 0; i < message.inserted.length; ++i)
                    $root.proto.Task.encode(message.inserted[i], writer.uint32(/* id 1, wireType 2 =*/10).fork()).ldelim();
            if (message.changed != null && message.changed.length)
                for (var i = 0; i < message.changed.length; ++i)
                    $root.proto.Task.encode(message.changed[i], writer.uint32(/* id 2, wireType 2 =*/18).fork()).ldelim();
            return writer;
        };

        /**
         * Encodes the specified ModifyResponse message, length delimited. Does not implicitly {@link proto.ModifyResponse.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.ModifyResponse
         * @static
         * @param {proto.IModifyResponse} message ModifyResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ModifyResponse.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a ModifyResponse message from the specified reader or buffer.
         * @function decode
         * @memberof proto.ModifyResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.ModifyResponse} ModifyResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ModifyResponse.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.ModifyResponse();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    if (!(message.inserted && message.inserted.length))
                        message.inserted = [];
                    message.inserted.push($root.proto.Task.decode(reader, reader.uint32()));
                    break;
                case 2:
                    if (!(message.changed && message.changed.length))
                        message.changed = [];
                    message.changed.push($root.proto.Task.decode(reader, reader.uint32()));
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a ModifyResponse message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.ModifyResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.ModifyResponse} ModifyResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ModifyResponse.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a ModifyResponse message.
         * @function verify
         * @memberof proto.ModifyResponse
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        ModifyResponse.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.inserted != null && message.hasOwnProperty("inserted")) {
                if (!Array.isArray(message.inserted))
                    return "inserted: array expected";
                for (var i = 0; i < message.inserted.length; ++i) {
                    var error = $root.proto.Task.verify(message.inserted[i]);
                    if (error)
                        return "inserted." + error;
                }
            }
            if (message.changed != null && message.hasOwnProperty("changed")) {
                if (!Array.isArray(message.changed))
                    return "changed: array expected";
                for (var i = 0; i < message.changed.length; ++i) {
                    var error = $root.proto.Task.verify(message.changed[i]);
                    if (error)
                        return "changed." + error;
                }
            }
            return null;
        };

        /**
         * Creates a ModifyResponse message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.ModifyResponse
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.ModifyResponse} ModifyResponse
         */
        ModifyResponse.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.ModifyResponse)
                return object;
            var message = new $root.proto.ModifyResponse();
            if (object.inserted) {
                if (!Array.isArray(object.inserted))
                    throw TypeError(".proto.ModifyResponse.inserted: array expected");
                message.inserted = [];
                for (var i = 0; i < object.inserted.length; ++i) {
                    if (typeof object.inserted[i] !== "object")
                        throw TypeError(".proto.ModifyResponse.inserted: object expected");
                    message.inserted[i] = $root.proto.Task.fromObject(object.inserted[i]);
                }
            }
            if (object.changed) {
                if (!Array.isArray(object.changed))
                    throw TypeError(".proto.ModifyResponse.changed: array expected");
                message.changed = [];
                for (var i = 0; i < object.changed.length; ++i) {
                    if (typeof object.changed[i] !== "object")
                        throw TypeError(".proto.ModifyResponse.changed: object expected");
                    message.changed[i] = $root.proto.Task.fromObject(object.changed[i]);
                }
            }
            return message;
        };

        /**
         * Creates a plain object from a ModifyResponse message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.ModifyResponse
         * @static
         * @param {proto.ModifyResponse} message ModifyResponse
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        ModifyResponse.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.arrays || options.defaults) {
                object.inserted = [];
                object.changed = [];
            }
            if (message.inserted && message.inserted.length) {
                object.inserted = [];
                for (var j = 0; j < message.inserted.length; ++j)
                    object.inserted[j] = $root.proto.Task.toObject(message.inserted[j], options);
            }
            if (message.changed && message.changed.length) {
                object.changed = [];
                for (var j = 0; j < message.changed.length; ++j)
                    object.changed[j] = $root.proto.Task.toObject(message.changed[j], options);
            }
            return object;
        };

        /**
         * Converts this ModifyResponse to JSON.
         * @function toJSON
         * @memberof proto.ModifyResponse
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        ModifyResponse.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return ModifyResponse;
    })();

    /**
     * ActionType enum.
     * @name proto.ActionType
     * @enum {number}
     * @property {number} CLAIM=0 CLAIM value
     * @property {number} DELETE=1 DELETE value
     * @property {number} CHANGE=2 CHANGE value
     * @property {number} DEPEND=3 DEPEND value
     * @property {number} DETAIL=4 DETAIL value
     * @property {number} INSERT=5 INSERT value
     * @property {number} READ=6 READ value
     */
    proto.ActionType = (function() {
        var valuesById = {}, values = Object.create(valuesById);
        values[valuesById[0] = "CLAIM"] = 0;
        values[valuesById[1] = "DELETE"] = 1;
        values[valuesById[2] = "CHANGE"] = 2;
        values[valuesById[3] = "DEPEND"] = 3;
        values[valuesById[4] = "DETAIL"] = 4;
        values[valuesById[5] = "INSERT"] = 5;
        values[valuesById[6] = "READ"] = 6;
        return values;
    })();

    proto.ModifyDep = (function() {

        /**
         * Properties of a ModifyDep.
         * @memberof proto
         * @interface IModifyDep
         * @property {proto.ActionType|null} [type] ModifyDep type
         * @property {proto.ITaskID|null} [id] ModifyDep id
         * @property {string|null} [msg] ModifyDep msg
         */

        /**
         * Constructs a new ModifyDep.
         * @memberof proto
         * @classdesc Represents a ModifyDep.
         * @implements IModifyDep
         * @constructor
         * @param {proto.IModifyDep=} [properties] Properties to set
         */
        function ModifyDep(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * ModifyDep type.
         * @member {proto.ActionType} type
         * @memberof proto.ModifyDep
         * @instance
         */
        ModifyDep.prototype.type = 0;

        /**
         * ModifyDep id.
         * @member {proto.ITaskID|null|undefined} id
         * @memberof proto.ModifyDep
         * @instance
         */
        ModifyDep.prototype.id = null;

        /**
         * ModifyDep msg.
         * @member {string} msg
         * @memberof proto.ModifyDep
         * @instance
         */
        ModifyDep.prototype.msg = "";

        /**
         * Creates a new ModifyDep instance using the specified properties.
         * @function create
         * @memberof proto.ModifyDep
         * @static
         * @param {proto.IModifyDep=} [properties] Properties to set
         * @returns {proto.ModifyDep} ModifyDep instance
         */
        ModifyDep.create = function create(properties) {
            return new ModifyDep(properties);
        };

        /**
         * Encodes the specified ModifyDep message. Does not implicitly {@link proto.ModifyDep.verify|verify} messages.
         * @function encode
         * @memberof proto.ModifyDep
         * @static
         * @param {proto.IModifyDep} message ModifyDep message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ModifyDep.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.type != null && Object.hasOwnProperty.call(message, "type"))
                writer.uint32(/* id 1, wireType 0 =*/8).int32(message.type);
            if (message.id != null && Object.hasOwnProperty.call(message, "id"))
                $root.proto.TaskID.encode(message.id, writer.uint32(/* id 2, wireType 2 =*/18).fork()).ldelim();
            if (message.msg != null && Object.hasOwnProperty.call(message, "msg"))
                writer.uint32(/* id 3, wireType 2 =*/26).string(message.msg);
            return writer;
        };

        /**
         * Encodes the specified ModifyDep message, length delimited. Does not implicitly {@link proto.ModifyDep.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.ModifyDep
         * @static
         * @param {proto.IModifyDep} message ModifyDep message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        ModifyDep.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a ModifyDep message from the specified reader or buffer.
         * @function decode
         * @memberof proto.ModifyDep
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.ModifyDep} ModifyDep
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ModifyDep.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.ModifyDep();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.type = reader.int32();
                    break;
                case 2:
                    message.id = $root.proto.TaskID.decode(reader, reader.uint32());
                    break;
                case 3:
                    message.msg = reader.string();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a ModifyDep message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.ModifyDep
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.ModifyDep} ModifyDep
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        ModifyDep.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a ModifyDep message.
         * @function verify
         * @memberof proto.ModifyDep
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        ModifyDep.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.type != null && message.hasOwnProperty("type"))
                switch (message.type) {
                default:
                    return "type: enum value expected";
                case 0:
                case 1:
                case 2:
                case 3:
                case 4:
                case 5:
                case 6:
                    break;
                }
            if (message.id != null && message.hasOwnProperty("id")) {
                var error = $root.proto.TaskID.verify(message.id);
                if (error)
                    return "id." + error;
            }
            if (message.msg != null && message.hasOwnProperty("msg"))
                if (!$util.isString(message.msg))
                    return "msg: string expected";
            return null;
        };

        /**
         * Creates a ModifyDep message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.ModifyDep
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.ModifyDep} ModifyDep
         */
        ModifyDep.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.ModifyDep)
                return object;
            var message = new $root.proto.ModifyDep();
            switch (object.type) {
            case "CLAIM":
            case 0:
                message.type = 0;
                break;
            case "DELETE":
            case 1:
                message.type = 1;
                break;
            case "CHANGE":
            case 2:
                message.type = 2;
                break;
            case "DEPEND":
            case 3:
                message.type = 3;
                break;
            case "DETAIL":
            case 4:
                message.type = 4;
                break;
            case "INSERT":
            case 5:
                message.type = 5;
                break;
            case "READ":
            case 6:
                message.type = 6;
                break;
            }
            if (object.id != null) {
                if (typeof object.id !== "object")
                    throw TypeError(".proto.ModifyDep.id: object expected");
                message.id = $root.proto.TaskID.fromObject(object.id);
            }
            if (object.msg != null)
                message.msg = String(object.msg);
            return message;
        };

        /**
         * Creates a plain object from a ModifyDep message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.ModifyDep
         * @static
         * @param {proto.ModifyDep} message ModifyDep
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        ModifyDep.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.defaults) {
                object.type = options.enums === String ? "CLAIM" : 0;
                object.id = null;
                object.msg = "";
            }
            if (message.type != null && message.hasOwnProperty("type"))
                object.type = options.enums === String ? $root.proto.ActionType[message.type] : message.type;
            if (message.id != null && message.hasOwnProperty("id"))
                object.id = $root.proto.TaskID.toObject(message.id, options);
            if (message.msg != null && message.hasOwnProperty("msg"))
                object.msg = message.msg;
            return object;
        };

        /**
         * Converts this ModifyDep to JSON.
         * @function toJSON
         * @memberof proto.ModifyDep
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        ModifyDep.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return ModifyDep;
    })();

    proto.AuthzDep = (function() {

        /**
         * Properties of an AuthzDep.
         * @memberof proto
         * @interface IAuthzDep
         * @property {Array.<proto.ActionType>|null} [actions] AuthzDep actions
         * @property {string|null} [exact] AuthzDep exact
         * @property {string|null} [prefix] AuthzDep prefix
         * @property {string|null} [msg] AuthzDep msg
         */

        /**
         * Constructs a new AuthzDep.
         * @memberof proto
         * @classdesc Represents an AuthzDep.
         * @implements IAuthzDep
         * @constructor
         * @param {proto.IAuthzDep=} [properties] Properties to set
         */
        function AuthzDep(properties) {
            this.actions = [];
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * AuthzDep actions.
         * @member {Array.<proto.ActionType>} actions
         * @memberof proto.AuthzDep
         * @instance
         */
        AuthzDep.prototype.actions = $util.emptyArray;

        /**
         * AuthzDep exact.
         * @member {string} exact
         * @memberof proto.AuthzDep
         * @instance
         */
        AuthzDep.prototype.exact = "";

        /**
         * AuthzDep prefix.
         * @member {string} prefix
         * @memberof proto.AuthzDep
         * @instance
         */
        AuthzDep.prototype.prefix = "";

        /**
         * AuthzDep msg.
         * @member {string} msg
         * @memberof proto.AuthzDep
         * @instance
         */
        AuthzDep.prototype.msg = "";

        /**
         * Creates a new AuthzDep instance using the specified properties.
         * @function create
         * @memberof proto.AuthzDep
         * @static
         * @param {proto.IAuthzDep=} [properties] Properties to set
         * @returns {proto.AuthzDep} AuthzDep instance
         */
        AuthzDep.create = function create(properties) {
            return new AuthzDep(properties);
        };

        /**
         * Encodes the specified AuthzDep message. Does not implicitly {@link proto.AuthzDep.verify|verify} messages.
         * @function encode
         * @memberof proto.AuthzDep
         * @static
         * @param {proto.IAuthzDep} message AuthzDep message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        AuthzDep.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.actions != null && message.actions.length) {
                writer.uint32(/* id 1, wireType 2 =*/10).fork();
                for (var i = 0; i < message.actions.length; ++i)
                    writer.int32(message.actions[i]);
                writer.ldelim();
            }
            if (message.exact != null && Object.hasOwnProperty.call(message, "exact"))
                writer.uint32(/* id 2, wireType 2 =*/18).string(message.exact);
            if (message.prefix != null && Object.hasOwnProperty.call(message, "prefix"))
                writer.uint32(/* id 3, wireType 2 =*/26).string(message.prefix);
            if (message.msg != null && Object.hasOwnProperty.call(message, "msg"))
                writer.uint32(/* id 4, wireType 2 =*/34).string(message.msg);
            return writer;
        };

        /**
         * Encodes the specified AuthzDep message, length delimited. Does not implicitly {@link proto.AuthzDep.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.AuthzDep
         * @static
         * @param {proto.IAuthzDep} message AuthzDep message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        AuthzDep.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes an AuthzDep message from the specified reader or buffer.
         * @function decode
         * @memberof proto.AuthzDep
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.AuthzDep} AuthzDep
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        AuthzDep.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.AuthzDep();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    if (!(message.actions && message.actions.length))
                        message.actions = [];
                    if ((tag & 7) === 2) {
                        var end2 = reader.uint32() + reader.pos;
                        while (reader.pos < end2)
                            message.actions.push(reader.int32());
                    } else
                        message.actions.push(reader.int32());
                    break;
                case 2:
                    message.exact = reader.string();
                    break;
                case 3:
                    message.prefix = reader.string();
                    break;
                case 4:
                    message.msg = reader.string();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes an AuthzDep message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.AuthzDep
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.AuthzDep} AuthzDep
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        AuthzDep.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies an AuthzDep message.
         * @function verify
         * @memberof proto.AuthzDep
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        AuthzDep.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.actions != null && message.hasOwnProperty("actions")) {
                if (!Array.isArray(message.actions))
                    return "actions: array expected";
                for (var i = 0; i < message.actions.length; ++i)
                    switch (message.actions[i]) {
                    default:
                        return "actions: enum value[] expected";
                    case 0:
                    case 1:
                    case 2:
                    case 3:
                    case 4:
                    case 5:
                    case 6:
                        break;
                    }
            }
            if (message.exact != null && message.hasOwnProperty("exact"))
                if (!$util.isString(message.exact))
                    return "exact: string expected";
            if (message.prefix != null && message.hasOwnProperty("prefix"))
                if (!$util.isString(message.prefix))
                    return "prefix: string expected";
            if (message.msg != null && message.hasOwnProperty("msg"))
                if (!$util.isString(message.msg))
                    return "msg: string expected";
            return null;
        };

        /**
         * Creates an AuthzDep message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.AuthzDep
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.AuthzDep} AuthzDep
         */
        AuthzDep.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.AuthzDep)
                return object;
            var message = new $root.proto.AuthzDep();
            if (object.actions) {
                if (!Array.isArray(object.actions))
                    throw TypeError(".proto.AuthzDep.actions: array expected");
                message.actions = [];
                for (var i = 0; i < object.actions.length; ++i)
                    switch (object.actions[i]) {
                    default:
                    case "CLAIM":
                    case 0:
                        message.actions[i] = 0;
                        break;
                    case "DELETE":
                    case 1:
                        message.actions[i] = 1;
                        break;
                    case "CHANGE":
                    case 2:
                        message.actions[i] = 2;
                        break;
                    case "DEPEND":
                    case 3:
                        message.actions[i] = 3;
                        break;
                    case "DETAIL":
                    case 4:
                        message.actions[i] = 4;
                        break;
                    case "INSERT":
                    case 5:
                        message.actions[i] = 5;
                        break;
                    case "READ":
                    case 6:
                        message.actions[i] = 6;
                        break;
                    }
            }
            if (object.exact != null)
                message.exact = String(object.exact);
            if (object.prefix != null)
                message.prefix = String(object.prefix);
            if (object.msg != null)
                message.msg = String(object.msg);
            return message;
        };

        /**
         * Creates a plain object from an AuthzDep message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.AuthzDep
         * @static
         * @param {proto.AuthzDep} message AuthzDep
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        AuthzDep.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.arrays || options.defaults)
                object.actions = [];
            if (options.defaults) {
                object.exact = "";
                object.prefix = "";
                object.msg = "";
            }
            if (message.actions && message.actions.length) {
                object.actions = [];
                for (var j = 0; j < message.actions.length; ++j)
                    object.actions[j] = options.enums === String ? $root.proto.ActionType[message.actions[j]] : message.actions[j];
            }
            if (message.exact != null && message.hasOwnProperty("exact"))
                object.exact = message.exact;
            if (message.prefix != null && message.hasOwnProperty("prefix"))
                object.prefix = message.prefix;
            if (message.msg != null && message.hasOwnProperty("msg"))
                object.msg = message.msg;
            return object;
        };

        /**
         * Converts this AuthzDep to JSON.
         * @function toJSON
         * @memberof proto.AuthzDep
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        AuthzDep.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return AuthzDep;
    })();

    proto.TasksRequest = (function() {

        /**
         * Properties of a TasksRequest.
         * @memberof proto
         * @interface ITasksRequest
         * @property {string|null} [claimantId] TasksRequest claimantId
         * @property {string|null} [queue] TasksRequest queue
         * @property {number|null} [limit] TasksRequest limit
         * @property {Array.<string>|null} [taskId] TasksRequest taskId
         * @property {boolean|null} [omitValues] TasksRequest omitValues
         */

        /**
         * Constructs a new TasksRequest.
         * @memberof proto
         * @classdesc Represents a TasksRequest.
         * @implements ITasksRequest
         * @constructor
         * @param {proto.ITasksRequest=} [properties] Properties to set
         */
        function TasksRequest(properties) {
            this.taskId = [];
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * TasksRequest claimantId.
         * @member {string} claimantId
         * @memberof proto.TasksRequest
         * @instance
         */
        TasksRequest.prototype.claimantId = "";

        /**
         * TasksRequest queue.
         * @member {string} queue
         * @memberof proto.TasksRequest
         * @instance
         */
        TasksRequest.prototype.queue = "";

        /**
         * TasksRequest limit.
         * @member {number} limit
         * @memberof proto.TasksRequest
         * @instance
         */
        TasksRequest.prototype.limit = 0;

        /**
         * TasksRequest taskId.
         * @member {Array.<string>} taskId
         * @memberof proto.TasksRequest
         * @instance
         */
        TasksRequest.prototype.taskId = $util.emptyArray;

        /**
         * TasksRequest omitValues.
         * @member {boolean} omitValues
         * @memberof proto.TasksRequest
         * @instance
         */
        TasksRequest.prototype.omitValues = false;

        /**
         * Creates a new TasksRequest instance using the specified properties.
         * @function create
         * @memberof proto.TasksRequest
         * @static
         * @param {proto.ITasksRequest=} [properties] Properties to set
         * @returns {proto.TasksRequest} TasksRequest instance
         */
        TasksRequest.create = function create(properties) {
            return new TasksRequest(properties);
        };

        /**
         * Encodes the specified TasksRequest message. Does not implicitly {@link proto.TasksRequest.verify|verify} messages.
         * @function encode
         * @memberof proto.TasksRequest
         * @static
         * @param {proto.ITasksRequest} message TasksRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TasksRequest.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.claimantId != null && Object.hasOwnProperty.call(message, "claimantId"))
                writer.uint32(/* id 1, wireType 2 =*/10).string(message.claimantId);
            if (message.queue != null && Object.hasOwnProperty.call(message, "queue"))
                writer.uint32(/* id 2, wireType 2 =*/18).string(message.queue);
            if (message.limit != null && Object.hasOwnProperty.call(message, "limit"))
                writer.uint32(/* id 3, wireType 0 =*/24).int32(message.limit);
            if (message.taskId != null && message.taskId.length)
                for (var i = 0; i < message.taskId.length; ++i)
                    writer.uint32(/* id 4, wireType 2 =*/34).string(message.taskId[i]);
            if (message.omitValues != null && Object.hasOwnProperty.call(message, "omitValues"))
                writer.uint32(/* id 5, wireType 0 =*/40).bool(message.omitValues);
            return writer;
        };

        /**
         * Encodes the specified TasksRequest message, length delimited. Does not implicitly {@link proto.TasksRequest.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.TasksRequest
         * @static
         * @param {proto.ITasksRequest} message TasksRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TasksRequest.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a TasksRequest message from the specified reader or buffer.
         * @function decode
         * @memberof proto.TasksRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.TasksRequest} TasksRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TasksRequest.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.TasksRequest();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.claimantId = reader.string();
                    break;
                case 2:
                    message.queue = reader.string();
                    break;
                case 3:
                    message.limit = reader.int32();
                    break;
                case 4:
                    if (!(message.taskId && message.taskId.length))
                        message.taskId = [];
                    message.taskId.push(reader.string());
                    break;
                case 5:
                    message.omitValues = reader.bool();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a TasksRequest message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.TasksRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.TasksRequest} TasksRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TasksRequest.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a TasksRequest message.
         * @function verify
         * @memberof proto.TasksRequest
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        TasksRequest.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.claimantId != null && message.hasOwnProperty("claimantId"))
                if (!$util.isString(message.claimantId))
                    return "claimantId: string expected";
            if (message.queue != null && message.hasOwnProperty("queue"))
                if (!$util.isString(message.queue))
                    return "queue: string expected";
            if (message.limit != null && message.hasOwnProperty("limit"))
                if (!$util.isInteger(message.limit))
                    return "limit: integer expected";
            if (message.taskId != null && message.hasOwnProperty("taskId")) {
                if (!Array.isArray(message.taskId))
                    return "taskId: array expected";
                for (var i = 0; i < message.taskId.length; ++i)
                    if (!$util.isString(message.taskId[i]))
                        return "taskId: string[] expected";
            }
            if (message.omitValues != null && message.hasOwnProperty("omitValues"))
                if (typeof message.omitValues !== "boolean")
                    return "omitValues: boolean expected";
            return null;
        };

        /**
         * Creates a TasksRequest message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.TasksRequest
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.TasksRequest} TasksRequest
         */
        TasksRequest.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.TasksRequest)
                return object;
            var message = new $root.proto.TasksRequest();
            if (object.claimantId != null)
                message.claimantId = String(object.claimantId);
            if (object.queue != null)
                message.queue = String(object.queue);
            if (object.limit != null)
                message.limit = object.limit | 0;
            if (object.taskId) {
                if (!Array.isArray(object.taskId))
                    throw TypeError(".proto.TasksRequest.taskId: array expected");
                message.taskId = [];
                for (var i = 0; i < object.taskId.length; ++i)
                    message.taskId[i] = String(object.taskId[i]);
            }
            if (object.omitValues != null)
                message.omitValues = Boolean(object.omitValues);
            return message;
        };

        /**
         * Creates a plain object from a TasksRequest message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.TasksRequest
         * @static
         * @param {proto.TasksRequest} message TasksRequest
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        TasksRequest.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.arrays || options.defaults)
                object.taskId = [];
            if (options.defaults) {
                object.claimantId = "";
                object.queue = "";
                object.limit = 0;
                object.omitValues = false;
            }
            if (message.claimantId != null && message.hasOwnProperty("claimantId"))
                object.claimantId = message.claimantId;
            if (message.queue != null && message.hasOwnProperty("queue"))
                object.queue = message.queue;
            if (message.limit != null && message.hasOwnProperty("limit"))
                object.limit = message.limit;
            if (message.taskId && message.taskId.length) {
                object.taskId = [];
                for (var j = 0; j < message.taskId.length; ++j)
                    object.taskId[j] = message.taskId[j];
            }
            if (message.omitValues != null && message.hasOwnProperty("omitValues"))
                object.omitValues = message.omitValues;
            return object;
        };

        /**
         * Converts this TasksRequest to JSON.
         * @function toJSON
         * @memberof proto.TasksRequest
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        TasksRequest.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return TasksRequest;
    })();

    proto.TasksResponse = (function() {

        /**
         * Properties of a TasksResponse.
         * @memberof proto
         * @interface ITasksResponse
         * @property {Array.<proto.ITask>|null} [tasks] TasksResponse tasks
         */

        /**
         * Constructs a new TasksResponse.
         * @memberof proto
         * @classdesc Represents a TasksResponse.
         * @implements ITasksResponse
         * @constructor
         * @param {proto.ITasksResponse=} [properties] Properties to set
         */
        function TasksResponse(properties) {
            this.tasks = [];
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * TasksResponse tasks.
         * @member {Array.<proto.ITask>} tasks
         * @memberof proto.TasksResponse
         * @instance
         */
        TasksResponse.prototype.tasks = $util.emptyArray;

        /**
         * Creates a new TasksResponse instance using the specified properties.
         * @function create
         * @memberof proto.TasksResponse
         * @static
         * @param {proto.ITasksResponse=} [properties] Properties to set
         * @returns {proto.TasksResponse} TasksResponse instance
         */
        TasksResponse.create = function create(properties) {
            return new TasksResponse(properties);
        };

        /**
         * Encodes the specified TasksResponse message. Does not implicitly {@link proto.TasksResponse.verify|verify} messages.
         * @function encode
         * @memberof proto.TasksResponse
         * @static
         * @param {proto.ITasksResponse} message TasksResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TasksResponse.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.tasks != null && message.tasks.length)
                for (var i = 0; i < message.tasks.length; ++i)
                    $root.proto.Task.encode(message.tasks[i], writer.uint32(/* id 1, wireType 2 =*/10).fork()).ldelim();
            return writer;
        };

        /**
         * Encodes the specified TasksResponse message, length delimited. Does not implicitly {@link proto.TasksResponse.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.TasksResponse
         * @static
         * @param {proto.ITasksResponse} message TasksResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TasksResponse.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a TasksResponse message from the specified reader or buffer.
         * @function decode
         * @memberof proto.TasksResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.TasksResponse} TasksResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TasksResponse.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.TasksResponse();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    if (!(message.tasks && message.tasks.length))
                        message.tasks = [];
                    message.tasks.push($root.proto.Task.decode(reader, reader.uint32()));
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a TasksResponse message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.TasksResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.TasksResponse} TasksResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TasksResponse.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a TasksResponse message.
         * @function verify
         * @memberof proto.TasksResponse
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        TasksResponse.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.tasks != null && message.hasOwnProperty("tasks")) {
                if (!Array.isArray(message.tasks))
                    return "tasks: array expected";
                for (var i = 0; i < message.tasks.length; ++i) {
                    var error = $root.proto.Task.verify(message.tasks[i]);
                    if (error)
                        return "tasks." + error;
                }
            }
            return null;
        };

        /**
         * Creates a TasksResponse message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.TasksResponse
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.TasksResponse} TasksResponse
         */
        TasksResponse.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.TasksResponse)
                return object;
            var message = new $root.proto.TasksResponse();
            if (object.tasks) {
                if (!Array.isArray(object.tasks))
                    throw TypeError(".proto.TasksResponse.tasks: array expected");
                message.tasks = [];
                for (var i = 0; i < object.tasks.length; ++i) {
                    if (typeof object.tasks[i] !== "object")
                        throw TypeError(".proto.TasksResponse.tasks: object expected");
                    message.tasks[i] = $root.proto.Task.fromObject(object.tasks[i]);
                }
            }
            return message;
        };

        /**
         * Creates a plain object from a TasksResponse message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.TasksResponse
         * @static
         * @param {proto.TasksResponse} message TasksResponse
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        TasksResponse.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.arrays || options.defaults)
                object.tasks = [];
            if (message.tasks && message.tasks.length) {
                object.tasks = [];
                for (var j = 0; j < message.tasks.length; ++j)
                    object.tasks[j] = $root.proto.Task.toObject(message.tasks[j], options);
            }
            return object;
        };

        /**
         * Converts this TasksResponse to JSON.
         * @function toJSON
         * @memberof proto.TasksResponse
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        TasksResponse.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return TasksResponse;
    })();

    proto.QueuesRequest = (function() {

        /**
         * Properties of a QueuesRequest.
         * @memberof proto
         * @interface IQueuesRequest
         * @property {Array.<string>|null} [matchPrefix] QueuesRequest matchPrefix
         * @property {Array.<string>|null} [matchExact] QueuesRequest matchExact
         * @property {number|null} [limit] QueuesRequest limit
         */

        /**
         * Constructs a new QueuesRequest.
         * @memberof proto
         * @classdesc Represents a QueuesRequest.
         * @implements IQueuesRequest
         * @constructor
         * @param {proto.IQueuesRequest=} [properties] Properties to set
         */
        function QueuesRequest(properties) {
            this.matchPrefix = [];
            this.matchExact = [];
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * QueuesRequest matchPrefix.
         * @member {Array.<string>} matchPrefix
         * @memberof proto.QueuesRequest
         * @instance
         */
        QueuesRequest.prototype.matchPrefix = $util.emptyArray;

        /**
         * QueuesRequest matchExact.
         * @member {Array.<string>} matchExact
         * @memberof proto.QueuesRequest
         * @instance
         */
        QueuesRequest.prototype.matchExact = $util.emptyArray;

        /**
         * QueuesRequest limit.
         * @member {number} limit
         * @memberof proto.QueuesRequest
         * @instance
         */
        QueuesRequest.prototype.limit = 0;

        /**
         * Creates a new QueuesRequest instance using the specified properties.
         * @function create
         * @memberof proto.QueuesRequest
         * @static
         * @param {proto.IQueuesRequest=} [properties] Properties to set
         * @returns {proto.QueuesRequest} QueuesRequest instance
         */
        QueuesRequest.create = function create(properties) {
            return new QueuesRequest(properties);
        };

        /**
         * Encodes the specified QueuesRequest message. Does not implicitly {@link proto.QueuesRequest.verify|verify} messages.
         * @function encode
         * @memberof proto.QueuesRequest
         * @static
         * @param {proto.IQueuesRequest} message QueuesRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        QueuesRequest.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.matchPrefix != null && message.matchPrefix.length)
                for (var i = 0; i < message.matchPrefix.length; ++i)
                    writer.uint32(/* id 1, wireType 2 =*/10).string(message.matchPrefix[i]);
            if (message.matchExact != null && message.matchExact.length)
                for (var i = 0; i < message.matchExact.length; ++i)
                    writer.uint32(/* id 2, wireType 2 =*/18).string(message.matchExact[i]);
            if (message.limit != null && Object.hasOwnProperty.call(message, "limit"))
                writer.uint32(/* id 3, wireType 0 =*/24).int32(message.limit);
            return writer;
        };

        /**
         * Encodes the specified QueuesRequest message, length delimited. Does not implicitly {@link proto.QueuesRequest.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.QueuesRequest
         * @static
         * @param {proto.IQueuesRequest} message QueuesRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        QueuesRequest.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a QueuesRequest message from the specified reader or buffer.
         * @function decode
         * @memberof proto.QueuesRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.QueuesRequest} QueuesRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        QueuesRequest.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.QueuesRequest();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    if (!(message.matchPrefix && message.matchPrefix.length))
                        message.matchPrefix = [];
                    message.matchPrefix.push(reader.string());
                    break;
                case 2:
                    if (!(message.matchExact && message.matchExact.length))
                        message.matchExact = [];
                    message.matchExact.push(reader.string());
                    break;
                case 3:
                    message.limit = reader.int32();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a QueuesRequest message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.QueuesRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.QueuesRequest} QueuesRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        QueuesRequest.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a QueuesRequest message.
         * @function verify
         * @memberof proto.QueuesRequest
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        QueuesRequest.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.matchPrefix != null && message.hasOwnProperty("matchPrefix")) {
                if (!Array.isArray(message.matchPrefix))
                    return "matchPrefix: array expected";
                for (var i = 0; i < message.matchPrefix.length; ++i)
                    if (!$util.isString(message.matchPrefix[i]))
                        return "matchPrefix: string[] expected";
            }
            if (message.matchExact != null && message.hasOwnProperty("matchExact")) {
                if (!Array.isArray(message.matchExact))
                    return "matchExact: array expected";
                for (var i = 0; i < message.matchExact.length; ++i)
                    if (!$util.isString(message.matchExact[i]))
                        return "matchExact: string[] expected";
            }
            if (message.limit != null && message.hasOwnProperty("limit"))
                if (!$util.isInteger(message.limit))
                    return "limit: integer expected";
            return null;
        };

        /**
         * Creates a QueuesRequest message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.QueuesRequest
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.QueuesRequest} QueuesRequest
         */
        QueuesRequest.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.QueuesRequest)
                return object;
            var message = new $root.proto.QueuesRequest();
            if (object.matchPrefix) {
                if (!Array.isArray(object.matchPrefix))
                    throw TypeError(".proto.QueuesRequest.matchPrefix: array expected");
                message.matchPrefix = [];
                for (var i = 0; i < object.matchPrefix.length; ++i)
                    message.matchPrefix[i] = String(object.matchPrefix[i]);
            }
            if (object.matchExact) {
                if (!Array.isArray(object.matchExact))
                    throw TypeError(".proto.QueuesRequest.matchExact: array expected");
                message.matchExact = [];
                for (var i = 0; i < object.matchExact.length; ++i)
                    message.matchExact[i] = String(object.matchExact[i]);
            }
            if (object.limit != null)
                message.limit = object.limit | 0;
            return message;
        };

        /**
         * Creates a plain object from a QueuesRequest message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.QueuesRequest
         * @static
         * @param {proto.QueuesRequest} message QueuesRequest
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        QueuesRequest.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.arrays || options.defaults) {
                object.matchPrefix = [];
                object.matchExact = [];
            }
            if (options.defaults)
                object.limit = 0;
            if (message.matchPrefix && message.matchPrefix.length) {
                object.matchPrefix = [];
                for (var j = 0; j < message.matchPrefix.length; ++j)
                    object.matchPrefix[j] = message.matchPrefix[j];
            }
            if (message.matchExact && message.matchExact.length) {
                object.matchExact = [];
                for (var j = 0; j < message.matchExact.length; ++j)
                    object.matchExact[j] = message.matchExact[j];
            }
            if (message.limit != null && message.hasOwnProperty("limit"))
                object.limit = message.limit;
            return object;
        };

        /**
         * Converts this QueuesRequest to JSON.
         * @function toJSON
         * @memberof proto.QueuesRequest
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        QueuesRequest.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return QueuesRequest;
    })();

    proto.QueuesResponse = (function() {

        /**
         * Properties of a QueuesResponse.
         * @memberof proto
         * @interface IQueuesResponse
         * @property {Array.<proto.IQueueStats>|null} [queues] QueuesResponse queues
         */

        /**
         * Constructs a new QueuesResponse.
         * @memberof proto
         * @classdesc Represents a QueuesResponse.
         * @implements IQueuesResponse
         * @constructor
         * @param {proto.IQueuesResponse=} [properties] Properties to set
         */
        function QueuesResponse(properties) {
            this.queues = [];
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * QueuesResponse queues.
         * @member {Array.<proto.IQueueStats>} queues
         * @memberof proto.QueuesResponse
         * @instance
         */
        QueuesResponse.prototype.queues = $util.emptyArray;

        /**
         * Creates a new QueuesResponse instance using the specified properties.
         * @function create
         * @memberof proto.QueuesResponse
         * @static
         * @param {proto.IQueuesResponse=} [properties] Properties to set
         * @returns {proto.QueuesResponse} QueuesResponse instance
         */
        QueuesResponse.create = function create(properties) {
            return new QueuesResponse(properties);
        };

        /**
         * Encodes the specified QueuesResponse message. Does not implicitly {@link proto.QueuesResponse.verify|verify} messages.
         * @function encode
         * @memberof proto.QueuesResponse
         * @static
         * @param {proto.IQueuesResponse} message QueuesResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        QueuesResponse.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.queues != null && message.queues.length)
                for (var i = 0; i < message.queues.length; ++i)
                    $root.proto.QueueStats.encode(message.queues[i], writer.uint32(/* id 1, wireType 2 =*/10).fork()).ldelim();
            return writer;
        };

        /**
         * Encodes the specified QueuesResponse message, length delimited. Does not implicitly {@link proto.QueuesResponse.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.QueuesResponse
         * @static
         * @param {proto.IQueuesResponse} message QueuesResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        QueuesResponse.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a QueuesResponse message from the specified reader or buffer.
         * @function decode
         * @memberof proto.QueuesResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.QueuesResponse} QueuesResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        QueuesResponse.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.QueuesResponse();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    if (!(message.queues && message.queues.length))
                        message.queues = [];
                    message.queues.push($root.proto.QueueStats.decode(reader, reader.uint32()));
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a QueuesResponse message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.QueuesResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.QueuesResponse} QueuesResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        QueuesResponse.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a QueuesResponse message.
         * @function verify
         * @memberof proto.QueuesResponse
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        QueuesResponse.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.queues != null && message.hasOwnProperty("queues")) {
                if (!Array.isArray(message.queues))
                    return "queues: array expected";
                for (var i = 0; i < message.queues.length; ++i) {
                    var error = $root.proto.QueueStats.verify(message.queues[i]);
                    if (error)
                        return "queues." + error;
                }
            }
            return null;
        };

        /**
         * Creates a QueuesResponse message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.QueuesResponse
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.QueuesResponse} QueuesResponse
         */
        QueuesResponse.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.QueuesResponse)
                return object;
            var message = new $root.proto.QueuesResponse();
            if (object.queues) {
                if (!Array.isArray(object.queues))
                    throw TypeError(".proto.QueuesResponse.queues: array expected");
                message.queues = [];
                for (var i = 0; i < object.queues.length; ++i) {
                    if (typeof object.queues[i] !== "object")
                        throw TypeError(".proto.QueuesResponse.queues: object expected");
                    message.queues[i] = $root.proto.QueueStats.fromObject(object.queues[i]);
                }
            }
            return message;
        };

        /**
         * Creates a plain object from a QueuesResponse message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.QueuesResponse
         * @static
         * @param {proto.QueuesResponse} message QueuesResponse
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        QueuesResponse.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.arrays || options.defaults)
                object.queues = [];
            if (message.queues && message.queues.length) {
                object.queues = [];
                for (var j = 0; j < message.queues.length; ++j)
                    object.queues[j] = $root.proto.QueueStats.toObject(message.queues[j], options);
            }
            return object;
        };

        /**
         * Converts this QueuesResponse to JSON.
         * @function toJSON
         * @memberof proto.QueuesResponse
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        QueuesResponse.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return QueuesResponse;
    })();

    proto.TimeRequest = (function() {

        /**
         * Properties of a TimeRequest.
         * @memberof proto
         * @interface ITimeRequest
         */

        /**
         * Constructs a new TimeRequest.
         * @memberof proto
         * @classdesc Represents a TimeRequest.
         * @implements ITimeRequest
         * @constructor
         * @param {proto.ITimeRequest=} [properties] Properties to set
         */
        function TimeRequest(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * Creates a new TimeRequest instance using the specified properties.
         * @function create
         * @memberof proto.TimeRequest
         * @static
         * @param {proto.ITimeRequest=} [properties] Properties to set
         * @returns {proto.TimeRequest} TimeRequest instance
         */
        TimeRequest.create = function create(properties) {
            return new TimeRequest(properties);
        };

        /**
         * Encodes the specified TimeRequest message. Does not implicitly {@link proto.TimeRequest.verify|verify} messages.
         * @function encode
         * @memberof proto.TimeRequest
         * @static
         * @param {proto.ITimeRequest} message TimeRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TimeRequest.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            return writer;
        };

        /**
         * Encodes the specified TimeRequest message, length delimited. Does not implicitly {@link proto.TimeRequest.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.TimeRequest
         * @static
         * @param {proto.ITimeRequest} message TimeRequest message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TimeRequest.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a TimeRequest message from the specified reader or buffer.
         * @function decode
         * @memberof proto.TimeRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.TimeRequest} TimeRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TimeRequest.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.TimeRequest();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a TimeRequest message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.TimeRequest
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.TimeRequest} TimeRequest
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TimeRequest.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a TimeRequest message.
         * @function verify
         * @memberof proto.TimeRequest
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        TimeRequest.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            return null;
        };

        /**
         * Creates a TimeRequest message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.TimeRequest
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.TimeRequest} TimeRequest
         */
        TimeRequest.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.TimeRequest)
                return object;
            return new $root.proto.TimeRequest();
        };

        /**
         * Creates a plain object from a TimeRequest message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.TimeRequest
         * @static
         * @param {proto.TimeRequest} message TimeRequest
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        TimeRequest.toObject = function toObject() {
            return {};
        };

        /**
         * Converts this TimeRequest to JSON.
         * @function toJSON
         * @memberof proto.TimeRequest
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        TimeRequest.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return TimeRequest;
    })();

    proto.TimeResponse = (function() {

        /**
         * Properties of a TimeResponse.
         * @memberof proto
         * @interface ITimeResponse
         * @property {number|Long|null} [timeMs] TimeResponse timeMs
         */

        /**
         * Constructs a new TimeResponse.
         * @memberof proto
         * @classdesc Represents a TimeResponse.
         * @implements ITimeResponse
         * @constructor
         * @param {proto.ITimeResponse=} [properties] Properties to set
         */
        function TimeResponse(properties) {
            if (properties)
                for (var keys = Object.keys(properties), i = 0; i < keys.length; ++i)
                    if (properties[keys[i]] != null)
                        this[keys[i]] = properties[keys[i]];
        }

        /**
         * TimeResponse timeMs.
         * @member {number|Long} timeMs
         * @memberof proto.TimeResponse
         * @instance
         */
        TimeResponse.prototype.timeMs = $util.Long ? $util.Long.fromBits(0,0,false) : 0;

        /**
         * Creates a new TimeResponse instance using the specified properties.
         * @function create
         * @memberof proto.TimeResponse
         * @static
         * @param {proto.ITimeResponse=} [properties] Properties to set
         * @returns {proto.TimeResponse} TimeResponse instance
         */
        TimeResponse.create = function create(properties) {
            return new TimeResponse(properties);
        };

        /**
         * Encodes the specified TimeResponse message. Does not implicitly {@link proto.TimeResponse.verify|verify} messages.
         * @function encode
         * @memberof proto.TimeResponse
         * @static
         * @param {proto.ITimeResponse} message TimeResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TimeResponse.encode = function encode(message, writer) {
            if (!writer)
                writer = $Writer.create();
            if (message.timeMs != null && Object.hasOwnProperty.call(message, "timeMs"))
                writer.uint32(/* id 1, wireType 0 =*/8).int64(message.timeMs);
            return writer;
        };

        /**
         * Encodes the specified TimeResponse message, length delimited. Does not implicitly {@link proto.TimeResponse.verify|verify} messages.
         * @function encodeDelimited
         * @memberof proto.TimeResponse
         * @static
         * @param {proto.ITimeResponse} message TimeResponse message or plain object to encode
         * @param {$protobuf.Writer} [writer] Writer to encode to
         * @returns {$protobuf.Writer} Writer
         */
        TimeResponse.encodeDelimited = function encodeDelimited(message, writer) {
            return this.encode(message, writer).ldelim();
        };

        /**
         * Decodes a TimeResponse message from the specified reader or buffer.
         * @function decode
         * @memberof proto.TimeResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @param {number} [length] Message length if known beforehand
         * @returns {proto.TimeResponse} TimeResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TimeResponse.decode = function decode(reader, length) {
            if (!(reader instanceof $Reader))
                reader = $Reader.create(reader);
            var end = length === undefined ? reader.len : reader.pos + length, message = new $root.proto.TimeResponse();
            while (reader.pos < end) {
                var tag = reader.uint32();
                switch (tag >>> 3) {
                case 1:
                    message.timeMs = reader.int64();
                    break;
                default:
                    reader.skipType(tag & 7);
                    break;
                }
            }
            return message;
        };

        /**
         * Decodes a TimeResponse message from the specified reader or buffer, length delimited.
         * @function decodeDelimited
         * @memberof proto.TimeResponse
         * @static
         * @param {$protobuf.Reader|Uint8Array} reader Reader or buffer to decode from
         * @returns {proto.TimeResponse} TimeResponse
         * @throws {Error} If the payload is not a reader or valid buffer
         * @throws {$protobuf.util.ProtocolError} If required fields are missing
         */
        TimeResponse.decodeDelimited = function decodeDelimited(reader) {
            if (!(reader instanceof $Reader))
                reader = new $Reader(reader);
            return this.decode(reader, reader.uint32());
        };

        /**
         * Verifies a TimeResponse message.
         * @function verify
         * @memberof proto.TimeResponse
         * @static
         * @param {Object.<string,*>} message Plain object to verify
         * @returns {string|null} `null` if valid, otherwise the reason why it is not
         */
        TimeResponse.verify = function verify(message) {
            if (typeof message !== "object" || message === null)
                return "object expected";
            if (message.timeMs != null && message.hasOwnProperty("timeMs"))
                if (!$util.isInteger(message.timeMs) && !(message.timeMs && $util.isInteger(message.timeMs.low) && $util.isInteger(message.timeMs.high)))
                    return "timeMs: integer|Long expected";
            return null;
        };

        /**
         * Creates a TimeResponse message from a plain object. Also converts values to their respective internal types.
         * @function fromObject
         * @memberof proto.TimeResponse
         * @static
         * @param {Object.<string,*>} object Plain object
         * @returns {proto.TimeResponse} TimeResponse
         */
        TimeResponse.fromObject = function fromObject(object) {
            if (object instanceof $root.proto.TimeResponse)
                return object;
            var message = new $root.proto.TimeResponse();
            if (object.timeMs != null)
                if ($util.Long)
                    (message.timeMs = $util.Long.fromValue(object.timeMs)).unsigned = false;
                else if (typeof object.timeMs === "string")
                    message.timeMs = parseInt(object.timeMs, 10);
                else if (typeof object.timeMs === "number")
                    message.timeMs = object.timeMs;
                else if (typeof object.timeMs === "object")
                    message.timeMs = new $util.LongBits(object.timeMs.low >>> 0, object.timeMs.high >>> 0).toNumber();
            return message;
        };

        /**
         * Creates a plain object from a TimeResponse message. Also converts values to other types if specified.
         * @function toObject
         * @memberof proto.TimeResponse
         * @static
         * @param {proto.TimeResponse} message TimeResponse
         * @param {$protobuf.IConversionOptions} [options] Conversion options
         * @returns {Object.<string,*>} Plain object
         */
        TimeResponse.toObject = function toObject(message, options) {
            if (!options)
                options = {};
            var object = {};
            if (options.defaults)
                if ($util.Long) {
                    var long = new $util.Long(0, 0, false);
                    object.timeMs = options.longs === String ? long.toString() : options.longs === Number ? long.toNumber() : long;
                } else
                    object.timeMs = options.longs === String ? "0" : 0;
            if (message.timeMs != null && message.hasOwnProperty("timeMs"))
                if (typeof message.timeMs === "number")
                    object.timeMs = options.longs === String ? String(message.timeMs) : message.timeMs;
                else
                    object.timeMs = options.longs === String ? $util.Long.prototype.toString.call(message.timeMs) : options.longs === Number ? new $util.LongBits(message.timeMs.low >>> 0, message.timeMs.high >>> 0).toNumber() : message.timeMs;
            return object;
        };

        /**
         * Converts this TimeResponse to JSON.
         * @function toJSON
         * @memberof proto.TimeResponse
         * @instance
         * @returns {Object.<string,*>} JSON object
         */
        TimeResponse.prototype.toJSON = function toJSON() {
            return this.constructor.toObject(this, $protobuf.util.toJSONOptions);
        };

        return TimeResponse;
    })();

    proto.EntroQ = (function() {

        /**
         * Constructs a new EntroQ service.
         * @memberof proto
         * @classdesc Represents an EntroQ
         * @extends $protobuf.rpc.Service
         * @constructor
         * @param {$protobuf.RPCImpl} rpcImpl RPC implementation
         * @param {boolean} [requestDelimited=false] Whether requests are length-delimited
         * @param {boolean} [responseDelimited=false] Whether responses are length-delimited
         */
        function EntroQ(rpcImpl, requestDelimited, responseDelimited) {
            $protobuf.rpc.Service.call(this, rpcImpl, requestDelimited, responseDelimited);
        }

        (EntroQ.prototype = Object.create($protobuf.rpc.Service.prototype)).constructor = EntroQ;

        /**
         * Creates new EntroQ service using the specified rpc implementation.
         * @function create
         * @memberof proto.EntroQ
         * @static
         * @param {$protobuf.RPCImpl} rpcImpl RPC implementation
         * @param {boolean} [requestDelimited=false] Whether requests are length-delimited
         * @param {boolean} [responseDelimited=false] Whether responses are length-delimited
         * @returns {EntroQ} RPC service. Useful where requests and/or responses are streamed.
         */
        EntroQ.create = function create(rpcImpl, requestDelimited, responseDelimited) {
            return new this(rpcImpl, requestDelimited, responseDelimited);
        };

        /**
         * Callback as used by {@link proto.EntroQ#tryClaim}.
         * @memberof proto.EntroQ
         * @typedef TryClaimCallback
         * @type {function}
         * @param {Error|null} error Error, if any
         * @param {proto.ClaimResponse} [response] ClaimResponse
         */

        /**
         * Calls TryClaim.
         * @function tryClaim
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IClaimRequest} request ClaimRequest message or plain object
         * @param {proto.EntroQ.TryClaimCallback} callback Node-style callback called with the error, if any, and ClaimResponse
         * @returns {undefined}
         * @variation 1
         */
        Object.defineProperty(EntroQ.prototype.tryClaim = function tryClaim(request, callback) {
            return this.rpcCall(tryClaim, $root.proto.ClaimRequest, $root.proto.ClaimResponse, request, callback);
        }, "name", { value: "TryClaim" });

        /**
         * Calls TryClaim.
         * @function tryClaim
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IClaimRequest} request ClaimRequest message or plain object
         * @returns {Promise<proto.ClaimResponse>} Promise
         * @variation 2
         */

        /**
         * Callback as used by {@link proto.EntroQ#claim}.
         * @memberof proto.EntroQ
         * @typedef ClaimCallback
         * @type {function}
         * @param {Error|null} error Error, if any
         * @param {proto.ClaimResponse} [response] ClaimResponse
         */

        /**
         * Calls Claim.
         * @function claim
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IClaimRequest} request ClaimRequest message or plain object
         * @param {proto.EntroQ.ClaimCallback} callback Node-style callback called with the error, if any, and ClaimResponse
         * @returns {undefined}
         * @variation 1
         */
        Object.defineProperty(EntroQ.prototype.claim = function claim(request, callback) {
            return this.rpcCall(claim, $root.proto.ClaimRequest, $root.proto.ClaimResponse, request, callback);
        }, "name", { value: "Claim" });

        /**
         * Calls Claim.
         * @function claim
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IClaimRequest} request ClaimRequest message or plain object
         * @returns {Promise<proto.ClaimResponse>} Promise
         * @variation 2
         */

        /**
         * Callback as used by {@link proto.EntroQ#modify}.
         * @memberof proto.EntroQ
         * @typedef ModifyCallback
         * @type {function}
         * @param {Error|null} error Error, if any
         * @param {proto.ModifyResponse} [response] ModifyResponse
         */

        /**
         * Calls Modify.
         * @function modify
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IModifyRequest} request ModifyRequest message or plain object
         * @param {proto.EntroQ.ModifyCallback} callback Node-style callback called with the error, if any, and ModifyResponse
         * @returns {undefined}
         * @variation 1
         */
        Object.defineProperty(EntroQ.prototype.modify = function modify(request, callback) {
            return this.rpcCall(modify, $root.proto.ModifyRequest, $root.proto.ModifyResponse, request, callback);
        }, "name", { value: "Modify" });

        /**
         * Calls Modify.
         * @function modify
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IModifyRequest} request ModifyRequest message or plain object
         * @returns {Promise<proto.ModifyResponse>} Promise
         * @variation 2
         */

        /**
         * Callback as used by {@link proto.EntroQ#tasks}.
         * @memberof proto.EntroQ
         * @typedef TasksCallback
         * @type {function}
         * @param {Error|null} error Error, if any
         * @param {proto.TasksResponse} [response] TasksResponse
         */

        /**
         * Calls Tasks.
         * @function tasks
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.ITasksRequest} request TasksRequest message or plain object
         * @param {proto.EntroQ.TasksCallback} callback Node-style callback called with the error, if any, and TasksResponse
         * @returns {undefined}
         * @variation 1
         */
        Object.defineProperty(EntroQ.prototype.tasks = function tasks(request, callback) {
            return this.rpcCall(tasks, $root.proto.TasksRequest, $root.proto.TasksResponse, request, callback);
        }, "name", { value: "Tasks" });

        /**
         * Calls Tasks.
         * @function tasks
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.ITasksRequest} request TasksRequest message or plain object
         * @returns {Promise<proto.TasksResponse>} Promise
         * @variation 2
         */

        /**
         * Callback as used by {@link proto.EntroQ#queues}.
         * @memberof proto.EntroQ
         * @typedef QueuesCallback
         * @type {function}
         * @param {Error|null} error Error, if any
         * @param {proto.QueuesResponse} [response] QueuesResponse
         */

        /**
         * Calls Queues.
         * @function queues
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IQueuesRequest} request QueuesRequest message or plain object
         * @param {proto.EntroQ.QueuesCallback} callback Node-style callback called with the error, if any, and QueuesResponse
         * @returns {undefined}
         * @variation 1
         */
        Object.defineProperty(EntroQ.prototype.queues = function queues(request, callback) {
            return this.rpcCall(queues, $root.proto.QueuesRequest, $root.proto.QueuesResponse, request, callback);
        }, "name", { value: "Queues" });

        /**
         * Calls Queues.
         * @function queues
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IQueuesRequest} request QueuesRequest message or plain object
         * @returns {Promise<proto.QueuesResponse>} Promise
         * @variation 2
         */

        /**
         * Callback as used by {@link proto.EntroQ#queueStats}.
         * @memberof proto.EntroQ
         * @typedef QueueStatsCallback
         * @type {function}
         * @param {Error|null} error Error, if any
         * @param {proto.QueuesResponse} [response] QueuesResponse
         */

        /**
         * Calls QueueStats.
         * @function queueStats
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IQueuesRequest} request QueuesRequest message or plain object
         * @param {proto.EntroQ.QueueStatsCallback} callback Node-style callback called with the error, if any, and QueuesResponse
         * @returns {undefined}
         * @variation 1
         */
        Object.defineProperty(EntroQ.prototype.queueStats = function queueStats(request, callback) {
            return this.rpcCall(queueStats, $root.proto.QueuesRequest, $root.proto.QueuesResponse, request, callback);
        }, "name", { value: "QueueStats" });

        /**
         * Calls QueueStats.
         * @function queueStats
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.IQueuesRequest} request QueuesRequest message or plain object
         * @returns {Promise<proto.QueuesResponse>} Promise
         * @variation 2
         */

        /**
         * Callback as used by {@link proto.EntroQ#time}.
         * @memberof proto.EntroQ
         * @typedef TimeCallback
         * @type {function}
         * @param {Error|null} error Error, if any
         * @param {proto.TimeResponse} [response] TimeResponse
         */

        /**
         * Calls Time.
         * @function time
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.ITimeRequest} request TimeRequest message or plain object
         * @param {proto.EntroQ.TimeCallback} callback Node-style callback called with the error, if any, and TimeResponse
         * @returns {undefined}
         * @variation 1
         */
        Object.defineProperty(EntroQ.prototype.time = function time(request, callback) {
            return this.rpcCall(time, $root.proto.TimeRequest, $root.proto.TimeResponse, request, callback);
        }, "name", { value: "Time" });

        /**
         * Calls Time.
         * @function time
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.ITimeRequest} request TimeRequest message or plain object
         * @returns {Promise<proto.TimeResponse>} Promise
         * @variation 2
         */

        /**
         * Callback as used by {@link proto.EntroQ#streamTasks}.
         * @memberof proto.EntroQ
         * @typedef StreamTasksCallback
         * @type {function}
         * @param {Error|null} error Error, if any
         * @param {proto.TasksResponse} [response] TasksResponse
         */

        /**
         * Calls StreamTasks.
         * @function streamTasks
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.ITasksRequest} request TasksRequest message or plain object
         * @param {proto.EntroQ.StreamTasksCallback} callback Node-style callback called with the error, if any, and TasksResponse
         * @returns {undefined}
         * @variation 1
         */
        Object.defineProperty(EntroQ.prototype.streamTasks = function streamTasks(request, callback) {
            return this.rpcCall(streamTasks, $root.proto.TasksRequest, $root.proto.TasksResponse, request, callback);
        }, "name", { value: "StreamTasks" });

        /**
         * Calls StreamTasks.
         * @function streamTasks
         * @memberof proto.EntroQ
         * @instance
         * @param {proto.ITasksRequest} request TasksRequest message or plain object
         * @returns {Promise<proto.TasksResponse>} Promise
         * @variation 2
         */

        return EntroQ;
    })();

    return proto;
})();

module.exports = $root;
