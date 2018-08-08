// Code generated by protoc-gen-go. DO NOT EDIT.
// source: entroq.proto

/*
Package proto is a generated protocol buffer package.

It is generated from these files:
	entroq.proto

It has these top-level messages:
	TaskID
	TaskData
	Task
	QueueStats
	ClaimRequest
	ClaimResponse
	ModifyRequest
	ModifyResponse
	TasksRequest
	TasksResponse
	QueuesRequest
	QueuesResponse
*/
package proto

import proto1 "github.com/golang/protobuf/proto"
import fmt "fmt"
import math "math"

import (
	context "golang.org/x/net/context"
	grpc "google.golang.org/grpc"
)

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto1.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto1.ProtoPackageIsVersion2 // please upgrade the proto package

// Status is returned for every response.
//
// If a response ends in UNKNOWN:
//    Something unexpected happened. It is usually safe to assume this is fatal.
//
// If a response ends in DEPENDENCY:
//    It is a final error: there is no way to satisfy this request ever again because
//    of the way that tasks are permanently removed (there will never be another task
//    with the same ID/Version again) when they are modified.
//
// If a response ends in TIMEOUT:
//    This means either the connection to the backend was dropped somehow, or that
//    there were no tasks available during the given wait time for the RPC (e.g.,
//    for a claim attempt).
type Status int32

const (
	Status_OK         Status = 0
	Status_UNKNOWN    Status = 1
	Status_DEPENDENCY Status = 2
	Status_TIMEOUT    Status = 3
)

var Status_name = map[int32]string{
	0: "OK",
	1: "UNKNOWN",
	2: "DEPENDENCY",
	3: "TIMEOUT",
}
var Status_value = map[string]int32{
	"OK":         0,
	"UNKNOWN":    1,
	"DEPENDENCY": 2,
	"TIMEOUT":    3,
}

func (x Status) String() string {
	return proto1.EnumName(Status_name, int32(x))
}
func (Status) EnumDescriptor() ([]byte, []int) { return fileDescriptor0, []int{0} }

// TaskID contains the ID and version of a task. Together these make a unique
// identifier for that task.
type TaskID struct {
	Id      string `protobuf:"bytes,1,opt,name=id" json:"id,omitempty"`
	Version int32  `protobuf:"varint,2,opt,name=version" json:"version,omitempty"`
}

func (m *TaskID) Reset()                    { *m = TaskID{} }
func (m *TaskID) String() string            { return proto1.CompactTextString(m) }
func (*TaskID) ProtoMessage()               {}
func (*TaskID) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{0} }

func (m *TaskID) GetId() string {
	if m != nil {
		return m.Id
	}
	return ""
}

func (m *TaskID) GetVersion() int32 {
	if m != nil {
		return m.Version
	}
	return 0
}

// TaskData contains only the data portion of a task. Useful for insertion.
type TaskData struct {
	// The name of the queue for this task.
	Queue string `protobuf:"bytes,1,opt,name=queue" json:"queue,omitempty"`
	// The epoch time in millis when this task becomes available.
	AtMs int64 `protobuf:"varint,2,opt,name=at_ms,json=atMs" json:"at_ms,omitempty"`
	// The task's opaque payload.
	Value []byte `protobuf:"bytes,3,opt,name=value,proto3" json:"value,omitempty"`
}

func (m *TaskData) Reset()                    { *m = TaskData{} }
func (m *TaskData) String() string            { return proto1.CompactTextString(m) }
func (*TaskData) ProtoMessage()               {}
func (*TaskData) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{1} }

func (m *TaskData) GetQueue() string {
	if m != nil {
		return m.Queue
	}
	return ""
}

func (m *TaskData) GetAtMs() int64 {
	if m != nil {
		return m.AtMs
	}
	return 0
}

func (m *TaskData) GetValue() []byte {
	if m != nil {
		return m.Value
	}
	return nil
}

// Task is a complete task object, containing IDs, data, and metadata.
type Task struct {
	// The name of the queue for this task.
	Queue   string `protobuf:"bytes,1,opt,name=queue" json:"queue,omitempty"`
	Id      string `protobuf:"bytes,2,opt,name=id" json:"id,omitempty"`
	Version int32  `protobuf:"varint,3,opt,name=version" json:"version,omitempty"`
	// The epoch time in millis when this task becomes available.
	AtMs int64 `protobuf:"varint,4,opt,name=at_ms,json=atMs" json:"at_ms,omitempty"`
	// The UUID representing the claimant (owner) for this task.
	ClaimantId string `protobuf:"bytes,5,opt,name=claimant_id,json=claimantId" json:"claimant_id,omitempty"`
	// The task's opaque payload.
	Value []byte `protobuf:"bytes,6,opt,name=value,proto3" json:"value,omitempty"`
	// Epoch times in millis for creation and update of this task.
	CreatedMs  int64 `protobuf:"varint,7,opt,name=created_ms,json=createdMs" json:"created_ms,omitempty"`
	ModifiedMs int64 `protobuf:"varint,8,opt,name=modified_ms,json=modifiedMs" json:"modified_ms,omitempty"`
}

func (m *Task) Reset()                    { *m = Task{} }
func (m *Task) String() string            { return proto1.CompactTextString(m) }
func (*Task) ProtoMessage()               {}
func (*Task) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{2} }

func (m *Task) GetQueue() string {
	if m != nil {
		return m.Queue
	}
	return ""
}

func (m *Task) GetId() string {
	if m != nil {
		return m.Id
	}
	return ""
}

func (m *Task) GetVersion() int32 {
	if m != nil {
		return m.Version
	}
	return 0
}

func (m *Task) GetAtMs() int64 {
	if m != nil {
		return m.AtMs
	}
	return 0
}

func (m *Task) GetClaimantId() string {
	if m != nil {
		return m.ClaimantId
	}
	return ""
}

func (m *Task) GetValue() []byte {
	if m != nil {
		return m.Value
	}
	return nil
}

func (m *Task) GetCreatedMs() int64 {
	if m != nil {
		return m.CreatedMs
	}
	return 0
}

func (m *Task) GetModifiedMs() int64 {
	if m != nil {
		return m.ModifiedMs
	}
	return 0
}

// QueueStats contains the name of the queue and the number of tasks within it.
type QueueStats struct {
	Name     string `protobuf:"bytes,1,opt,name=name" json:"name,omitempty"`
	NumTasks int32  `protobuf:"varint,2,opt,name=num_tasks,json=numTasks" json:"num_tasks,omitempty"`
}

func (m *QueueStats) Reset()                    { *m = QueueStats{} }
func (m *QueueStats) String() string            { return proto1.CompactTextString(m) }
func (*QueueStats) ProtoMessage()               {}
func (*QueueStats) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{3} }

func (m *QueueStats) GetName() string {
	if m != nil {
		return m.Name
	}
	return ""
}

func (m *QueueStats) GetNumTasks() int32 {
	if m != nil {
		return m.NumTasks
	}
	return 0
}

// ClaimRequest is sent to attempt to claim a task from a queue. The claimant ID
// should be unique to the requesting worker (e.g., if multiple workers are in
// the same process, they should all have different claimant IDs assigned).
type ClaimRequest struct {
	ClaimantId string `protobuf:"bytes,1,opt,name=claimant_id,json=claimantId" json:"claimant_id,omitempty"`
	Queue      string `protobuf:"bytes,2,opt,name=queue" json:"queue,omitempty"`
	DurationMs int64  `protobuf:"varint,3,opt,name=duration_ms,json=durationMs" json:"duration_ms,omitempty"`
}

func (m *ClaimRequest) Reset()                    { *m = ClaimRequest{} }
func (m *ClaimRequest) String() string            { return proto1.CompactTextString(m) }
func (*ClaimRequest) ProtoMessage()               {}
func (*ClaimRequest) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{4} }

func (m *ClaimRequest) GetClaimantId() string {
	if m != nil {
		return m.ClaimantId
	}
	return ""
}

func (m *ClaimRequest) GetQueue() string {
	if m != nil {
		return m.Queue
	}
	return ""
}

func (m *ClaimRequest) GetDurationMs() int64 {
	if m != nil {
		return m.DurationMs
	}
	return 0
}

// ClaimResponse is returned when a claim is fulfilled or becomes obviously impossible.
// A successful claim results in a valid Task message, and status will be OK.
type ClaimResponse struct {
	Status Status `protobuf:"varint,1,opt,name=status,enum=proto.Status" json:"status,omitempty"`
	Task   *Task  `protobuf:"bytes,2,opt,name=task" json:"task,omitempty"`
	Error  string `protobuf:"bytes,3,opt,name=error" json:"error,omitempty"`
}

func (m *ClaimResponse) Reset()                    { *m = ClaimResponse{} }
func (m *ClaimResponse) String() string            { return proto1.CompactTextString(m) }
func (*ClaimResponse) ProtoMessage()               {}
func (*ClaimResponse) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{5} }

func (m *ClaimResponse) GetStatus() Status {
	if m != nil {
		return m.Status
	}
	return Status_OK
}

func (m *ClaimResponse) GetTask() *Task {
	if m != nil {
		return m.Task
	}
	return nil
}

func (m *ClaimResponse) GetError() string {
	if m != nil {
		return m.Error
	}
	return ""
}

// ModifyRequest sends a request to modify a set of tasks with given
// dependencies. It is performed in a transaction, in which either all
// suggested modifications succeed and all dependencies are satisfied, or
// nothing is committed at all. A failure due to dependencies (in any
// of changes, deletes, or inserts) will be permanent.
//
// All successful changes will cause the requester to be come the claimant.
type ModifyRequest struct {
	ClaimantId string      `protobuf:"bytes,1,opt,name=claimant_id,json=claimantId" json:"claimant_id,omitempty"`
	Inserts    []*TaskData `protobuf:"bytes,2,rep,name=inserts" json:"inserts,omitempty"`
	Changes    []*Task     `protobuf:"bytes,3,rep,name=changes" json:"changes,omitempty"`
	Deletes    []*TaskID   `protobuf:"bytes,4,rep,name=deletes" json:"deletes,omitempty"`
	Depends    []*TaskID   `protobuf:"bytes,5,rep,name=depends" json:"depends,omitempty"`
}

func (m *ModifyRequest) Reset()                    { *m = ModifyRequest{} }
func (m *ModifyRequest) String() string            { return proto1.CompactTextString(m) }
func (*ModifyRequest) ProtoMessage()               {}
func (*ModifyRequest) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{6} }

func (m *ModifyRequest) GetClaimantId() string {
	if m != nil {
		return m.ClaimantId
	}
	return ""
}

func (m *ModifyRequest) GetInserts() []*TaskData {
	if m != nil {
		return m.Inserts
	}
	return nil
}

func (m *ModifyRequest) GetChanges() []*Task {
	if m != nil {
		return m.Changes
	}
	return nil
}

func (m *ModifyRequest) GetDeletes() []*TaskID {
	if m != nil {
		return m.Deletes
	}
	return nil
}

func (m *ModifyRequest) GetDepends() []*TaskID {
	if m != nil {
		return m.Depends
	}
	return nil
}

// ModifyResponse returns inserted and updated tasks when successful, or
// an error representing all failures when not.
type ModifyResponse struct {
	Status   Status  `protobuf:"varint,1,opt,name=status,enum=proto.Status" json:"status,omitempty"`
	Inserted []*Task `protobuf:"bytes,2,rep,name=inserted" json:"inserted,omitempty"`
	Changed  []*Task `protobuf:"bytes,3,rep,name=changed" json:"changed,omitempty"`
	Error    string  `protobuf:"bytes,4,opt,name=error" json:"error,omitempty"`
}

func (m *ModifyResponse) Reset()                    { *m = ModifyResponse{} }
func (m *ModifyResponse) String() string            { return proto1.CompactTextString(m) }
func (*ModifyResponse) ProtoMessage()               {}
func (*ModifyResponse) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{7} }

func (m *ModifyResponse) GetStatus() Status {
	if m != nil {
		return m.Status
	}
	return Status_OK
}

func (m *ModifyResponse) GetInserted() []*Task {
	if m != nil {
		return m.Inserted
	}
	return nil
}

func (m *ModifyResponse) GetChanged() []*Task {
	if m != nil {
		return m.Changed
	}
	return nil
}

func (m *ModifyResponse) GetError() string {
	if m != nil {
		return m.Error
	}
	return ""
}

// TasksRequest is sent to request a complete listing of tasks for the
// given queue. If claimant_id is empty, all tasks (not just expired
// or owned tasks) are returned.
type TasksRequest struct {
	ClaimantId string `protobuf:"bytes,1,opt,name=claimant_id,json=claimantId" json:"claimant_id,omitempty"`
	Queue      string `protobuf:"bytes,2,opt,name=queue" json:"queue,omitempty"`
}

func (m *TasksRequest) Reset()                    { *m = TasksRequest{} }
func (m *TasksRequest) String() string            { return proto1.CompactTextString(m) }
func (*TasksRequest) ProtoMessage()               {}
func (*TasksRequest) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{8} }

func (m *TasksRequest) GetClaimantId() string {
	if m != nil {
		return m.ClaimantId
	}
	return ""
}

func (m *TasksRequest) GetQueue() string {
	if m != nil {
		return m.Queue
	}
	return ""
}

// TasksReqponse contains the tasks requested.
type TasksResponse struct {
	Status Status  `protobuf:"varint,1,opt,name=status,enum=proto.Status" json:"status,omitempty"`
	Tasks  []*Task `protobuf:"bytes,2,rep,name=tasks" json:"tasks,omitempty"`
	Error  string  `protobuf:"bytes,3,opt,name=error" json:"error,omitempty"`
}

func (m *TasksResponse) Reset()                    { *m = TasksResponse{} }
func (m *TasksResponse) String() string            { return proto1.CompactTextString(m) }
func (*TasksResponse) ProtoMessage()               {}
func (*TasksResponse) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{9} }

func (m *TasksResponse) GetStatus() Status {
	if m != nil {
		return m.Status
	}
	return Status_OK
}

func (m *TasksResponse) GetTasks() []*Task {
	if m != nil {
		return m.Tasks
	}
	return nil
}

func (m *TasksResponse) GetError() string {
	if m != nil {
		return m.Error
	}
	return ""
}

// QueuesRequest is sent to request a listing of all known queues.
type QueuesRequest struct {
}

func (m *QueuesRequest) Reset()                    { *m = QueuesRequest{} }
func (m *QueuesRequest) String() string            { return proto1.CompactTextString(m) }
func (*QueuesRequest) ProtoMessage()               {}
func (*QueuesRequest) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{10} }

// QueuesResponse contains the requested list of queue statistics.
type QueuesResponse struct {
	Status Status        `protobuf:"varint,1,opt,name=status,enum=proto.Status" json:"status,omitempty"`
	Queues []*QueueStats `protobuf:"bytes,2,rep,name=queues" json:"queues,omitempty"`
	Error  string        `protobuf:"bytes,3,opt,name=error" json:"error,omitempty"`
}

func (m *QueuesResponse) Reset()                    { *m = QueuesResponse{} }
func (m *QueuesResponse) String() string            { return proto1.CompactTextString(m) }
func (*QueuesResponse) ProtoMessage()               {}
func (*QueuesResponse) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{11} }

func (m *QueuesResponse) GetStatus() Status {
	if m != nil {
		return m.Status
	}
	return Status_OK
}

func (m *QueuesResponse) GetQueues() []*QueueStats {
	if m != nil {
		return m.Queues
	}
	return nil
}

func (m *QueuesResponse) GetError() string {
	if m != nil {
		return m.Error
	}
	return ""
}

func init() {
	proto1.RegisterType((*TaskID)(nil), "proto.TaskID")
	proto1.RegisterType((*TaskData)(nil), "proto.TaskData")
	proto1.RegisterType((*Task)(nil), "proto.Task")
	proto1.RegisterType((*QueueStats)(nil), "proto.QueueStats")
	proto1.RegisterType((*ClaimRequest)(nil), "proto.ClaimRequest")
	proto1.RegisterType((*ClaimResponse)(nil), "proto.ClaimResponse")
	proto1.RegisterType((*ModifyRequest)(nil), "proto.ModifyRequest")
	proto1.RegisterType((*ModifyResponse)(nil), "proto.ModifyResponse")
	proto1.RegisterType((*TasksRequest)(nil), "proto.TasksRequest")
	proto1.RegisterType((*TasksResponse)(nil), "proto.TasksResponse")
	proto1.RegisterType((*QueuesRequest)(nil), "proto.QueuesRequest")
	proto1.RegisterType((*QueuesResponse)(nil), "proto.QueuesResponse")
	proto1.RegisterEnum("proto.Status", Status_name, Status_value)
}

// Reference imports to suppress errors if they are not otherwise used.
var _ context.Context
var _ grpc.ClientConn

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
const _ = grpc.SupportPackageIsVersion4

// Client API for EntroQ service

type EntroQClient interface {
	Claim(ctx context.Context, in *ClaimRequest, opts ...grpc.CallOption) (*ClaimResponse, error)
	Modify(ctx context.Context, in *ModifyRequest, opts ...grpc.CallOption) (*ModifyResponse, error)
	Tasks(ctx context.Context, in *TasksRequest, opts ...grpc.CallOption) (*TasksResponse, error)
	Queues(ctx context.Context, in *QueuesRequest, opts ...grpc.CallOption) (*QueuesResponse, error)
}

type entroQClient struct {
	cc *grpc.ClientConn
}

func NewEntroQClient(cc *grpc.ClientConn) EntroQClient {
	return &entroQClient{cc}
}

func (c *entroQClient) Claim(ctx context.Context, in *ClaimRequest, opts ...grpc.CallOption) (*ClaimResponse, error) {
	out := new(ClaimResponse)
	err := grpc.Invoke(ctx, "/proto.EntroQ/Claim", in, out, c.cc, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *entroQClient) Modify(ctx context.Context, in *ModifyRequest, opts ...grpc.CallOption) (*ModifyResponse, error) {
	out := new(ModifyResponse)
	err := grpc.Invoke(ctx, "/proto.EntroQ/Modify", in, out, c.cc, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *entroQClient) Tasks(ctx context.Context, in *TasksRequest, opts ...grpc.CallOption) (*TasksResponse, error) {
	out := new(TasksResponse)
	err := grpc.Invoke(ctx, "/proto.EntroQ/Tasks", in, out, c.cc, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *entroQClient) Queues(ctx context.Context, in *QueuesRequest, opts ...grpc.CallOption) (*QueuesResponse, error) {
	out := new(QueuesResponse)
	err := grpc.Invoke(ctx, "/proto.EntroQ/Queues", in, out, c.cc, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Server API for EntroQ service

type EntroQServer interface {
	Claim(context.Context, *ClaimRequest) (*ClaimResponse, error)
	Modify(context.Context, *ModifyRequest) (*ModifyResponse, error)
	Tasks(context.Context, *TasksRequest) (*TasksResponse, error)
	Queues(context.Context, *QueuesRequest) (*QueuesResponse, error)
}

func RegisterEntroQServer(s *grpc.Server, srv EntroQServer) {
	s.RegisterService(&_EntroQ_serviceDesc, srv)
}

func _EntroQ_Claim_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ClaimRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EntroQServer).Claim(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/proto.EntroQ/Claim",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(EntroQServer).Claim(ctx, req.(*ClaimRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _EntroQ_Modify_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ModifyRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EntroQServer).Modify(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/proto.EntroQ/Modify",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(EntroQServer).Modify(ctx, req.(*ModifyRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _EntroQ_Tasks_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(TasksRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EntroQServer).Tasks(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/proto.EntroQ/Tasks",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(EntroQServer).Tasks(ctx, req.(*TasksRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _EntroQ_Queues_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(QueuesRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EntroQServer).Queues(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/proto.EntroQ/Queues",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(EntroQServer).Queues(ctx, req.(*QueuesRequest))
	}
	return interceptor(ctx, in, info, handler)
}

var _EntroQ_serviceDesc = grpc.ServiceDesc{
	ServiceName: "proto.EntroQ",
	HandlerType: (*EntroQServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Claim",
			Handler:    _EntroQ_Claim_Handler,
		},
		{
			MethodName: "Modify",
			Handler:    _EntroQ_Modify_Handler,
		},
		{
			MethodName: "Tasks",
			Handler:    _EntroQ_Tasks_Handler,
		},
		{
			MethodName: "Queues",
			Handler:    _EntroQ_Queues_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "entroq.proto",
}

func init() { proto1.RegisterFile("entroq.proto", fileDescriptor0) }

var fileDescriptor0 = []byte{
	// 652 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0xa4, 0x54, 0x4f, 0x4f, 0xdb, 0x4e,
	0x10, 0xfd, 0xd9, 0x8e, 0x9d, 0x64, 0x1c, 0x87, 0xfc, 0x16, 0x2a, 0x59, 0x54, 0x15, 0xd4, 0x12,
	0x22, 0xf4, 0xc0, 0xc1, 0x15, 0x97, 0x4a, 0x3d, 0x11, 0x1f, 0x22, 0x94, 0x50, 0x0c, 0xa8, 0xea,
	0x09, 0x6d, 0xd9, 0xa5, 0xb5, 0xc0, 0x36, 0x78, 0xd7, 0xa8, 0xfd, 0x30, 0xfd, 0x52, 0x55, 0xcf,
	0xfd, 0x2c, 0xd5, 0xfe, 0x23, 0x76, 0x0a, 0x55, 0xa3, 0x9e, 0x92, 0x9d, 0x37, 0x3b, 0xf3, 0xde,
	0xdb, 0x19, 0xc3, 0x80, 0x16, 0xbc, 0x2a, 0xef, 0xf6, 0x6f, 0xab, 0x92, 0x97, 0xc8, 0x95, 0x3f,
	0x51, 0x0c, 0xde, 0x19, 0x66, 0xd7, 0xd3, 0x09, 0x1a, 0x82, 0x9d, 0x91, 0xd0, 0xda, 0xb6, 0xc6,
	0xfd, 0xd4, 0xce, 0x08, 0x0a, 0xa1, 0x7b, 0x4f, 0x2b, 0x96, 0x95, 0x45, 0x68, 0x6f, 0x5b, 0x63,
	0x37, 0x35, 0xc7, 0xe8, 0x08, 0x7a, 0xe2, 0xce, 0x04, 0x73, 0x8c, 0x36, 0xc0, 0xbd, 0xab, 0x69,
	0x4d, 0xf5, 0x45, 0x75, 0x40, 0xeb, 0xe0, 0x62, 0x7e, 0x91, 0x33, 0x79, 0xd3, 0x49, 0x3b, 0x98,
	0xcf, 0x98, 0x48, 0xbd, 0xc7, 0x37, 0x35, 0x0d, 0x9d, 0x6d, 0x6b, 0x3c, 0x48, 0xd5, 0x21, 0xfa,
	0x6e, 0x41, 0x47, 0x54, 0x7b, 0xa2, 0x92, 0x62, 0x65, 0x3f, 0xc6, 0xca, 0x69, 0xb1, 0x5a, 0xf4,
	0xec, 0x34, 0x7a, 0x6e, 0x81, 0x7f, 0x79, 0x83, 0xb3, 0x1c, 0x17, 0xfc, 0x22, 0x23, 0xa1, 0x2b,
	0xeb, 0x80, 0x09, 0x4d, 0xc9, 0x82, 0x94, 0xd7, 0x20, 0x85, 0x5e, 0x00, 0x5c, 0x56, 0x14, 0x73,
	0x4a, 0x44, 0xc1, 0xae, 0x2c, 0xd8, 0xd7, 0x11, 0x55, 0x35, 0x2f, 0x49, 0x76, 0x95, 0x29, 0xbc,
	0x27, 0x71, 0x30, 0xa1, 0x19, 0x8b, 0xde, 0x02, 0x9c, 0x08, 0xfa, 0xa7, 0x1c, 0x73, 0x86, 0x10,
	0x74, 0x0a, 0x9c, 0x1b, 0x61, 0xf2, 0x3f, 0x7a, 0x0e, 0xfd, 0xa2, 0xce, 0x2f, 0x38, 0x66, 0xd7,
	0x4c, 0xfb, 0xdb, 0x2b, 0xea, 0x5c, 0x38, 0xc1, 0xa2, 0x2b, 0x18, 0x1c, 0x0a, 0x8a, 0x29, 0xbd,
	0xab, 0x29, 0xe3, 0xcb, 0x2a, 0xac, 0xc7, 0x54, 0x28, 0xef, 0xec, 0xa6, 0x77, 0x5b, 0xe0, 0x93,
	0xba, 0xc2, 0x3c, 0x2b, 0x0b, 0x41, 0xd3, 0x51, 0x34, 0x4d, 0x68, 0xc6, 0xa2, 0x1c, 0x02, 0xdd,
	0x87, 0xdd, 0x96, 0x05, 0xa3, 0x68, 0x07, 0x3c, 0xc6, 0x31, 0xaf, 0x99, 0xec, 0x31, 0x8c, 0x03,
	0x35, 0x2c, 0xfb, 0xa7, 0x32, 0x98, 0x6a, 0x10, 0x6d, 0x41, 0x47, 0x10, 0x97, 0xdd, 0xfc, 0xd8,
	0xd7, 0x49, 0x82, 0x7b, 0x2a, 0x01, 0xc1, 0x87, 0x56, 0x55, 0x59, 0xc9, 0x9e, 0xfd, 0x54, 0x1d,
	0xa2, 0x1f, 0x16, 0x04, 0x33, 0x61, 0xd2, 0xd7, 0xbf, 0x16, 0xb6, 0x07, 0xdd, 0xac, 0x60, 0xb4,
	0xe2, 0xc2, 0x24, 0x67, 0xec, 0xc7, 0x6b, 0x8d, 0x66, 0x62, 0x00, 0x53, 0x83, 0xa3, 0x1d, 0xe8,
	0x5e, 0x7e, 0xc6, 0xc5, 0x27, 0x2a, 0x94, 0x3a, 0xcb, 0xbc, 0x0c, 0x86, 0x76, 0xa1, 0x4b, 0xe8,
	0x0d, 0xe5, 0x54, 0x0c, 0x8a, 0x48, 0x0b, 0x1a, 0x69, 0xd3, 0x49, 0x6a, 0x50, 0x95, 0x78, 0x4b,
	0x0b, 0xc2, 0x42, 0xf7, 0x89, 0x44, 0x89, 0x46, 0xdf, 0x2c, 0x18, 0x1a, 0x59, 0xab, 0xf9, 0xb8,
	0x0b, 0x3d, 0xc5, 0x9e, 0x12, 0x2d, 0xaf, 0xc5, 0xf9, 0x01, 0x5c, 0x68, 0x23, 0x7f, 0xd0, 0x46,
	0x16, 0xb6, 0x77, 0x9a, 0xb6, 0x27, 0x30, 0x90, 0x63, 0xf5, 0x6f, 0xd3, 0x14, 0x95, 0x10, 0xe8,
	0x32, 0xab, 0x89, 0x7c, 0x09, 0xae, 0x99, 0xf2, 0xdf, 0x98, 0x2b, 0xe4, 0x89, 0x71, 0x59, 0x83,
	0x40, 0x2e, 0x91, 0x21, 0x1e, 0x7d, 0x81, 0xa1, 0x09, 0xac, 0x46, 0x61, 0x0f, 0x3c, 0xa9, 0xc1,
	0x70, 0xf8, 0x5f, 0xa7, 0x2d, 0x76, 0x34, 0xd5, 0x09, 0x8f, 0x53, 0x79, 0xf5, 0x06, 0x3c, 0x55,
	0x12, 0x79, 0x60, 0x1f, 0x1f, 0x8d, 0xfe, 0x43, 0x3e, 0x74, 0xcf, 0xe7, 0x47, 0xf3, 0xe3, 0xf7,
	0xf3, 0x91, 0x85, 0x86, 0x00, 0x93, 0xe4, 0x5d, 0x32, 0x9f, 0x24, 0xf3, 0xc3, 0x0f, 0x23, 0x5b,
	0x80, 0x67, 0xd3, 0x59, 0x72, 0x7c, 0x7e, 0x36, 0x72, 0xe2, 0x9f, 0x16, 0x78, 0x89, 0xf8, 0xf2,
	0x9e, 0xa0, 0x18, 0x5c, 0xb9, 0x6f, 0x68, 0x5d, 0x13, 0x68, 0x6e, 0xf9, 0xe6, 0x46, 0x3b, 0xa8,
	0x25, 0x1e, 0x80, 0xa7, 0x86, 0x0b, 0x19, 0xbc, 0xb5, 0x42, 0x9b, 0xcf, 0x96, 0xa2, 0xfa, 0x5a,
	0x0c, 0xae, 0x7c, 0xad, 0x87, 0x56, 0xcd, 0x11, 0x78, 0x68, 0xd5, 0x7e, 0xd0, 0x03, 0xf0, 0x4e,
	0xb4, 0x0b, 0x4d, 0x83, 0xd8, 0x72, 0xab, 0xf6, 0x23, 0x7c, 0xf4, 0x64, 0xf4, 0xf5, 0xaf, 0x00,
	0x00, 0x00, 0xff, 0xff, 0x8c, 0x92, 0x7e, 0x4d, 0x60, 0x06, 0x00, 0x00,
}
