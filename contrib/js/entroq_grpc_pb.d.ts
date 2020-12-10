// package: proto
// file: entroq.proto

import * as grpc from 'grpc';
import * as entroq_pb from './entroq_pb';

interface IEntroQService extends grpc.ServiceDefinition<grpc.UntypedServiceImplementation> {
  tryClaim: IEntroQService_ITryClaim;
  claim: IEntroQService_IClaim;
  modify: IEntroQService_IModify;
  tasks: IEntroQService_ITasks;
  queues: IEntroQService_IQueues;
  queueStats: IEntroQService_IQueueStats;
  time: IEntroQService_ITime;
  streamTasks: IEntroQService_IStreamTasks;
}

interface IEntroQService_ITryClaim {
  path: string; // "/proto.EntroQ/TryClaim"
  requestStream: boolean; // false
  responseStream: boolean; // false
  requestSerialize: grpc.serialize<entroq_pb.ClaimRequest>;
  requestDeserialize: grpc.deserialize<entroq_pb.ClaimRequest>;
  responseSerialize: grpc.serialize<entroq_pb.ClaimResponse>;
  responseDeserialize: grpc.deserialize<entroq_pb.ClaimResponse>;
}

interface IEntroQService_IClaim {
  path: string; // "/proto.EntroQ/Claim"
  requestStream: boolean; // false
  responseStream: boolean; // false
  requestSerialize: grpc.serialize<entroq_pb.ClaimRequest>;
  requestDeserialize: grpc.deserialize<entroq_pb.ClaimRequest>;
  responseSerialize: grpc.serialize<entroq_pb.ClaimResponse>;
  responseDeserialize: grpc.deserialize<entroq_pb.ClaimResponse>;
}

interface IEntroQService_IModify {
  path: string; // "/proto.EntroQ/Modify"
  requestStream: boolean; // false
  responseStream: boolean; // false
  requestSerialize: grpc.serialize<entroq_pb.ModifyRequest>;
  requestDeserialize: grpc.deserialize<entroq_pb.ModifyRequest>;
  responseSerialize: grpc.serialize<entroq_pb.ModifyResponse>;
  responseDeserialize: grpc.deserialize<entroq_pb.ModifyResponse>;
}

interface IEntroQService_ITasks {
  path: string; // "/proto.EntroQ/Tasks"
  requestStream: boolean; // false
  responseStream: boolean; // false
  requestSerialize: grpc.serialize<entroq_pb.TasksRequest>;
  requestDeserialize: grpc.deserialize<entroq_pb.TasksRequest>;
  responseSerialize: grpc.serialize<entroq_pb.TasksResponse>;
  responseDeserialize: grpc.deserialize<entroq_pb.TasksResponse>;
}

interface IEntroQService_IQueues {
  path: string; // "/proto.EntroQ/Queues"
  requestStream: boolean; // false
  responseStream: boolean; // false
  requestSerialize: grpc.serialize<entroq_pb.QueuesRequest>;
  requestDeserialize: grpc.deserialize<entroq_pb.QueuesRequest>;
  responseSerialize: grpc.serialize<entroq_pb.QueuesResponse>;
  responseDeserialize: grpc.deserialize<entroq_pb.QueuesResponse>;
}

interface IEntroQService_IQueueStats {
  path: string; // "/proto.EntroQ/QueueStats"
  requestStream: boolean; // false
  responseStream: boolean; // false
  requestSerialize: grpc.serialize<entroq_pb.QueuesRequest>;
  requestDeserialize: grpc.deserialize<entroq_pb.QueuesRequest>;
  responseSerialize: grpc.serialize<entroq_pb.QueuesResponse>;
  responseDeserialize: grpc.deserialize<entroq_pb.QueuesResponse>;
}

interface IEntroQService_ITime {
  path: string; // "/proto.EntroQ/Time"
  requestStream: boolean; // false
  responseStream: boolean; // false
  requestSerialize: grpc.serialize<entroq_pb.TimeRequest>;
  requestDeserialize: grpc.deserialize<entroq_pb.TimeRequest>;
  responseSerialize: grpc.serialize<entroq_pb.TimeResponse>;
  responseDeserialize: grpc.deserialize<entroq_pb.TimeResponse>;
}

interface IEntroQService_IStreamTasks {
  path: string; // "/proto.EntroQ/StreamTasks"
  requestStream: boolean; // false
  responseStream: boolean; // true
  requestSerialize: grpc.serialize<entroq_pb.TasksRequest>;
  requestDeserialize: grpc.deserialize<entroq_pb.TasksRequest>;
  responseSerialize: grpc.serialize<entroq_pb.Task>;
  responseDeserialize: grpc.deserialize<entroq_pb.Task>;
}

export const EntroQService: IEntroQService;
export interface IEntroQServer {
  tryClaim: grpc.handleUnaryCall<entroq_pb.ClaimRequest, entroq_pb.ClaimResponse>;
  claim: grpc.handleUnaryCall<entroq_pb.ClaimRequest, entroq_pb.ClaimResponse>;
  modify: grpc.handleUnaryCall<entroq_pb.ModifyRequest, entroq_pb.ModifyResponse>;
  tasks: grpc.handleUnaryCall<entroq_pb.TasksRequest, entroq_pb.TasksResponse>;
  queues: grpc.handleUnaryCall<entroq_pb.QueuesRequest, entroq_pb.QueuesResponse>;
  queueStats: grpc.handleUnaryCall<entroq_pb.QueuesRequest, entroq_pb.QueuesResponse>;
  time: grpc.handleUnaryCall<entroq_pb.TimeRequest, entroq_pb.TimeResponse>;
  streamTasks: grpc.handleServerStreamingCall<entroq_pb.TasksRequest, entroq_pb.Task>;
}

export interface IEntroQClient {
  tryClaim(request: entroq_pb.ClaimRequest, callback: (error: Error | null, response: entroq_pb.ClaimResponse) => void): grpc.ClientUnaryCall;
  tryClaim(request: entroq_pb.ClaimRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.ClaimResponse) => void): grpc.ClientUnaryCall;
  claim(request: entroq_pb.ClaimRequest, callback: (error: Error | null, response: entroq_pb.ClaimResponse) => void): grpc.ClientUnaryCall;
  claim(request: entroq_pb.ClaimRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.ClaimResponse) => void): grpc.ClientUnaryCall;
  modify(request: entroq_pb.ModifyRequest, callback: (error: Error | null, response: entroq_pb.ModifyResponse) => void): grpc.ClientUnaryCall;
  modify(request: entroq_pb.ModifyRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.ModifyResponse) => void): grpc.ClientUnaryCall;
  tasks(request: entroq_pb.TasksRequest, callback: (error: Error | null, response: entroq_pb.TasksResponse) => void): grpc.ClientUnaryCall;
  tasks(request: entroq_pb.TasksRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.TasksResponse) => void): grpc.ClientUnaryCall;
  queues(request: entroq_pb.QueuesRequest, callback: (error: Error | null, response: entroq_pb.QueuesResponse) => void): grpc.ClientUnaryCall;
  queues(request: entroq_pb.QueuesRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.QueuesResponse) => void): grpc.ClientUnaryCall;
  queueStats(request: entroq_pb.QueuesRequest, callback: (error: Error | null, response: entroq_pb.QueuesResponse) => void): grpc.ClientUnaryCall;
  queueStats(request: entroq_pb.QueuesRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.QueuesResponse) => void): grpc.ClientUnaryCall;
  time(request: entroq_pb.TimeRequest, callback: (error: Error | null, response: entroq_pb.TimeResponse) => void): grpc.ClientUnaryCall;
  time(request: entroq_pb.TimeRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.TimeResponse) => void): grpc.ClientUnaryCall;
  streamTasks(request: entroq_pb.TasksRequest, metadata?: grpc.Metadata): grpc.ClientReadableStream<entroq_pb.Task>;
}

export class EntroQClient extends grpc.Client implements IEntroQClient {
  constructor(address: string, credentials: grpc.ChannelCredentials, options?: object);
  public tryClaim(request: entroq_pb.ClaimRequest, callback: (error: Error | null, response: entroq_pb.ClaimResponse) => void): grpc.ClientUnaryCall;
  public tryClaim(request: entroq_pb.ClaimRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.ClaimResponse) => void): grpc.ClientUnaryCall;
  public claim(request: entroq_pb.ClaimRequest, callback: (error: Error | null, response: entroq_pb.ClaimResponse) => void): grpc.ClientUnaryCall;
  public claim(request: entroq_pb.ClaimRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.ClaimResponse) => void): grpc.ClientUnaryCall;
  public modify(request: entroq_pb.ModifyRequest, callback: (error: Error | null, response: entroq_pb.ModifyResponse) => void): grpc.ClientUnaryCall;
  public modify(request: entroq_pb.ModifyRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.ModifyResponse) => void): grpc.ClientUnaryCall;
  public tasks(request: entroq_pb.TasksRequest, callback: (error: Error | null, response: entroq_pb.TasksResponse) => void): grpc.ClientUnaryCall;
  public tasks(request: entroq_pb.TasksRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.TasksResponse) => void): grpc.ClientUnaryCall;
  public queues(request: entroq_pb.QueuesRequest, callback: (error: Error | null, response: entroq_pb.QueuesResponse) => void): grpc.ClientUnaryCall;
  public queues(request: entroq_pb.QueuesRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.QueuesResponse) => void): grpc.ClientUnaryCall;
  public queueStats(request: entroq_pb.QueuesRequest, callback: (error: Error | null, response: entroq_pb.QueuesResponse) => void): grpc.ClientUnaryCall;
  public queueStats(request: entroq_pb.QueuesRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.QueuesResponse) => void): grpc.ClientUnaryCall;
  public time(request: entroq_pb.TimeRequest, callback: (error: Error | null, response: entroq_pb.TimeResponse) => void): grpc.ClientUnaryCall;
  public time(request: entroq_pb.TimeRequest, metadata: grpc.Metadata, callback: (error: Error | null, response: entroq_pb.TimeResponse) => void): grpc.ClientUnaryCall;
  public streamTasks(request: entroq_pb.TasksRequest, metadata?: grpc.Metadata): grpc.ClientReadableStream<entroq_pb.Task>;
}

