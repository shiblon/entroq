// GENERATED CODE -- DO NOT EDIT!

'use strict';
var grpc = require('grpc');
var entroq_pb = require('./entroq_pb.js');

function serialize_proto_ClaimRequest(arg) {
  if (!(arg instanceof entroq_pb.ClaimRequest)) {
    throw new Error('Expected argument of type proto.ClaimRequest');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_ClaimRequest(buffer_arg) {
  return entroq_pb.ClaimRequest.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_ClaimResponse(arg) {
  if (!(arg instanceof entroq_pb.ClaimResponse)) {
    throw new Error('Expected argument of type proto.ClaimResponse');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_ClaimResponse(buffer_arg) {
  return entroq_pb.ClaimResponse.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_ModifyRequest(arg) {
  if (!(arg instanceof entroq_pb.ModifyRequest)) {
    throw new Error('Expected argument of type proto.ModifyRequest');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_ModifyRequest(buffer_arg) {
  return entroq_pb.ModifyRequest.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_ModifyResponse(arg) {
  if (!(arg instanceof entroq_pb.ModifyResponse)) {
    throw new Error('Expected argument of type proto.ModifyResponse');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_ModifyResponse(buffer_arg) {
  return entroq_pb.ModifyResponse.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_QueuesRequest(arg) {
  if (!(arg instanceof entroq_pb.QueuesRequest)) {
    throw new Error('Expected argument of type proto.QueuesRequest');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_QueuesRequest(buffer_arg) {
  return entroq_pb.QueuesRequest.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_QueuesResponse(arg) {
  if (!(arg instanceof entroq_pb.QueuesResponse)) {
    throw new Error('Expected argument of type proto.QueuesResponse');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_QueuesResponse(buffer_arg) {
  return entroq_pb.QueuesResponse.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_Task(arg) {
  if (!(arg instanceof entroq_pb.Task)) {
    throw new Error('Expected argument of type proto.Task');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_Task(buffer_arg) {
  return entroq_pb.Task.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_TasksRequest(arg) {
  if (!(arg instanceof entroq_pb.TasksRequest)) {
    throw new Error('Expected argument of type proto.TasksRequest');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_TasksRequest(buffer_arg) {
  return entroq_pb.TasksRequest.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_TasksResponse(arg) {
  if (!(arg instanceof entroq_pb.TasksResponse)) {
    throw new Error('Expected argument of type proto.TasksResponse');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_TasksResponse(buffer_arg) {
  return entroq_pb.TasksResponse.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_TimeRequest(arg) {
  if (!(arg instanceof entroq_pb.TimeRequest)) {
    throw new Error('Expected argument of type proto.TimeRequest');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_TimeRequest(buffer_arg) {
  return entroq_pb.TimeRequest.deserializeBinary(new Uint8Array(buffer_arg));
}

function serialize_proto_TimeResponse(arg) {
  if (!(arg instanceof entroq_pb.TimeResponse)) {
    throw new Error('Expected argument of type proto.TimeResponse');
  }
  return Buffer.from(arg.serializeBinary());
}

function deserialize_proto_TimeResponse(buffer_arg) {
  return entroq_pb.TimeResponse.deserializeBinary(new Uint8Array(buffer_arg));
}


// EntroQ is a service that allows for access to a task queue.
var EntroQService = exports.EntroQService = {
  tryClaim: {
    path: '/proto.EntroQ/TryClaim',
    requestStream: false,
    responseStream: false,
    requestType: entroq_pb.ClaimRequest,
    responseType: entroq_pb.ClaimResponse,
    requestSerialize: serialize_proto_ClaimRequest,
    requestDeserialize: deserialize_proto_ClaimRequest,
    responseSerialize: serialize_proto_ClaimResponse,
    responseDeserialize: deserialize_proto_ClaimResponse,
  },
  claim: {
    path: '/proto.EntroQ/Claim',
    requestStream: false,
    responseStream: false,
    requestType: entroq_pb.ClaimRequest,
    responseType: entroq_pb.ClaimResponse,
    requestSerialize: serialize_proto_ClaimRequest,
    requestDeserialize: deserialize_proto_ClaimRequest,
    responseSerialize: serialize_proto_ClaimResponse,
    responseDeserialize: deserialize_proto_ClaimResponse,
  },
  modify: {
    path: '/proto.EntroQ/Modify',
    requestStream: false,
    responseStream: false,
    requestType: entroq_pb.ModifyRequest,
    responseType: entroq_pb.ModifyResponse,
    requestSerialize: serialize_proto_ModifyRequest,
    requestDeserialize: deserialize_proto_ModifyRequest,
    responseSerialize: serialize_proto_ModifyResponse,
    responseDeserialize: deserialize_proto_ModifyResponse,
  },
  tasks: {
    path: '/proto.EntroQ/Tasks',
    requestStream: false,
    responseStream: false,
    requestType: entroq_pb.TasksRequest,
    responseType: entroq_pb.TasksResponse,
    requestSerialize: serialize_proto_TasksRequest,
    requestDeserialize: deserialize_proto_TasksRequest,
    responseSerialize: serialize_proto_TasksResponse,
    responseDeserialize: deserialize_proto_TasksResponse,
  },
  queues: {
    path: '/proto.EntroQ/Queues',
    requestStream: false,
    responseStream: false,
    requestType: entroq_pb.QueuesRequest,
    responseType: entroq_pb.QueuesResponse,
    requestSerialize: serialize_proto_QueuesRequest,
    requestDeserialize: deserialize_proto_QueuesRequest,
    responseSerialize: serialize_proto_QueuesResponse,
    responseDeserialize: deserialize_proto_QueuesResponse,
  },
  queueStats: {
    path: '/proto.EntroQ/QueueStats',
    requestStream: false,
    responseStream: false,
    requestType: entroq_pb.QueuesRequest,
    responseType: entroq_pb.QueuesResponse,
    requestSerialize: serialize_proto_QueuesRequest,
    requestDeserialize: deserialize_proto_QueuesRequest,
    responseSerialize: serialize_proto_QueuesResponse,
    responseDeserialize: deserialize_proto_QueuesResponse,
  },
  time: {
    path: '/proto.EntroQ/Time',
    requestStream: false,
    responseStream: false,
    requestType: entroq_pb.TimeRequest,
    responseType: entroq_pb.TimeResponse,
    requestSerialize: serialize_proto_TimeRequest,
    requestDeserialize: deserialize_proto_TimeRequest,
    responseSerialize: serialize_proto_TimeResponse,
    responseDeserialize: deserialize_proto_TimeResponse,
  },
  streamTasks: {
    path: '/proto.EntroQ/StreamTasks',
    requestStream: false,
    responseStream: true,
    requestType: entroq_pb.TasksRequest,
    responseType: entroq_pb.Task,
    requestSerialize: serialize_proto_TasksRequest,
    requestDeserialize: deserialize_proto_TasksRequest,
    responseSerialize: serialize_proto_Task,
    responseDeserialize: deserialize_proto_Task,
  },
};

exports.EntroQClient = grpc.makeGenericClientConstructor(EntroQService);
