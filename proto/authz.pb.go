// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.24.0-devel
// 	protoc        v3.14.0
// source: authz.proto

package proto

import (
	proto "github.com/golang/protobuf/proto"
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

// This is a compile-time assertion that a sufficiently up-to-date version
// of the legacy proto package is being used.
const _ = proto.ProtoPackageIsVersion4

type AuthzQueue_Action int32

const (
	AuthzQueue_READ   AuthzQueue_Action = 0
	AuthzQueue_CLAIM  AuthzQueue_Action = 1
	AuthzQueue_DELETE AuthzQueue_Action = 2 // Also used for "change from" this queue.
	AuthzQueue_INSERT AuthzQueue_Action = 4 // Also used for "change to" this queue.
	AuthzQueue_CHANGE AuthzQueue_Action = 8
	AuthzQueue_ANY    AuthzQueue_Action = 65535
)

// Enum value maps for AuthzQueue_Action.
var (
	AuthzQueue_Action_name = map[int32]string{
		0:     "READ",
		1:     "CLAIM",
		2:     "DELETE",
		4:     "INSERT",
		8:     "CHANGE",
		65535: "ANY",
	}
	AuthzQueue_Action_value = map[string]int32{
		"READ":   0,
		"CLAIM":  1,
		"DELETE": 2,
		"INSERT": 4,
		"CHANGE": 8,
		"ANY":    65535,
	}
)

func (x AuthzQueue_Action) Enum() *AuthzQueue_Action {
	p := new(AuthzQueue_Action)
	*p = x
	return p
}

func (x AuthzQueue_Action) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (AuthzQueue_Action) Descriptor() protoreflect.EnumDescriptor {
	return file_authz_proto_enumTypes[0].Descriptor()
}

func (AuthzQueue_Action) Type() protoreflect.EnumType {
	return &file_authz_proto_enumTypes[0]
}

func (x AuthzQueue_Action) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use AuthzQueue_Action.Descriptor instead.
func (AuthzQueue_Action) EnumDescriptor() ([]byte, []int) {
	return file_authz_proto_rawDescGZIP(), []int{1, 0}
}

// Authz contains info about authorization. Usually just a token.
type Authz struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Token string `protobuf:"bytes,1,opt,name=token,proto3" json:"token,omitempty"`
}

func (x *Authz) Reset() {
	*x = Authz{}
	if protoimpl.UnsafeEnabled {
		mi := &file_authz_proto_msgTypes[0]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *Authz) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Authz) ProtoMessage() {}

func (x *Authz) ProtoReflect() protoreflect.Message {
	mi := &file_authz_proto_msgTypes[0]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Authz.ProtoReflect.Descriptor instead.
func (*Authz) Descriptor() ([]byte, []int) {
	return file_authz_proto_rawDescGZIP(), []int{0}
}

func (x *Authz) GetToken() string {
	if x != nil {
		return x.Token
	}
	return ""
}

// AuthzQueue contains a single queue specification (name, pattern, etc.)
type AuthzQueue struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Exact  string `protobuf:"bytes,1,opt,name=exact,proto3" json:"exact,omitempty"`
	Prefix string `protobuf:"bytes,2,opt,name=prefix,proto3" json:"prefix,omitempty"`
	// Action indicates what this queue is being used for (e.g., CLAIM).
	Actions []AuthzQueue_Action `protobuf:"varint,3,rep,packed,name=actions,proto3,enum=proto.AuthzQueue_Action" json:"actions,omitempty"`
}

func (x *AuthzQueue) Reset() {
	*x = AuthzQueue{}
	if protoimpl.UnsafeEnabled {
		mi := &file_authz_proto_msgTypes[1]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *AuthzQueue) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AuthzQueue) ProtoMessage() {}

func (x *AuthzQueue) ProtoReflect() protoreflect.Message {
	mi := &file_authz_proto_msgTypes[1]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AuthzQueue.ProtoReflect.Descriptor instead.
func (*AuthzQueue) Descriptor() ([]byte, []int) {
	return file_authz_proto_rawDescGZIP(), []int{1}
}

func (x *AuthzQueue) GetExact() string {
	if x != nil {
		return x.Exact
	}
	return ""
}

func (x *AuthzQueue) GetPrefix() string {
	if x != nil {
		return x.Prefix
	}
	return ""
}

func (x *AuthzQueue) GetActions() []AuthzQueue_Action {
	if x != nil {
		return x.Actions
	}
	return nil
}

// AuthzRequest contains authorization information and information about what
// the user is attempting to do with which queues. Useful for subrequest
// authorization (e.g., sending to OpenPolicyAgent).
type AuthzRequest struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Authz  *Authz        `protobuf:"bytes,1,opt,name=authz,proto3" json:"authz,omitempty"`
	Queues []*AuthzQueue `protobuf:"bytes,2,rep,name=queues,proto3" json:"queues,omitempty"`
}

func (x *AuthzRequest) Reset() {
	*x = AuthzRequest{}
	if protoimpl.UnsafeEnabled {
		mi := &file_authz_proto_msgTypes[2]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *AuthzRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AuthzRequest) ProtoMessage() {}

func (x *AuthzRequest) ProtoReflect() protoreflect.Message {
	mi := &file_authz_proto_msgTypes[2]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AuthzRequest.ProtoReflect.Descriptor instead.
func (*AuthzRequest) Descriptor() ([]byte, []int) {
	return file_authz_proto_rawDescGZIP(), []int{2}
}

func (x *AuthzRequest) GetAuthz() *Authz {
	if x != nil {
		return x.Authz
	}
	return nil
}

func (x *AuthzRequest) GetQueues() []*AuthzQueue {
	if x != nil {
		return x.Queues
	}
	return nil
}

// AuthzResponse is the authorization response, containing disallowed queues
// with their actions. If the disallowed_queues field is empty, this action is
// allowed.
type AuthzResponse struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	DisallowedQueues []*AuthzQueue `protobuf:"bytes,1,rep,name=disallowed_queues,json=disallowedQueues,proto3" json:"disallowed_queues,omitempty"`
}

func (x *AuthzResponse) Reset() {
	*x = AuthzResponse{}
	if protoimpl.UnsafeEnabled {
		mi := &file_authz_proto_msgTypes[3]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *AuthzResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AuthzResponse) ProtoMessage() {}

func (x *AuthzResponse) ProtoReflect() protoreflect.Message {
	mi := &file_authz_proto_msgTypes[3]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AuthzResponse.ProtoReflect.Descriptor instead.
func (*AuthzResponse) Descriptor() ([]byte, []int) {
	return file_authz_proto_rawDescGZIP(), []int{3}
}

func (x *AuthzResponse) GetDisallowedQueues() []*AuthzQueue {
	if x != nil {
		return x.DisallowedQueues
	}
	return nil
}

// AuthzEntity contains information about a person or a group.
type AuthzEntity struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// Name is used to identify this entity unambiguously (user name, user ID).
	Name string `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	// Queues contains information about what this user is allowed to do with
	// queues, queue matches, etc.
	Queues []*AuthzQueue `protobuf:"bytes,2,rep,name=queues,proto3" json:"queues,omitempty"`
}

func (x *AuthzEntity) Reset() {
	*x = AuthzEntity{}
	if protoimpl.UnsafeEnabled {
		mi := &file_authz_proto_msgTypes[4]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *AuthzEntity) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AuthzEntity) ProtoMessage() {}

func (x *AuthzEntity) ProtoReflect() protoreflect.Message {
	mi := &file_authz_proto_msgTypes[4]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AuthzEntity.ProtoReflect.Descriptor instead.
func (*AuthzEntity) Descriptor() ([]byte, []int) {
	return file_authz_proto_rawDescGZIP(), []int{4}
}

func (x *AuthzEntity) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *AuthzEntity) GetQueues() []*AuthzQueue {
	if x != nil {
		return x.Queues
	}
	return nil
}

// AuthzSpec contains information about what is allowed and disallowed for users, roles, etc.
// This is used as configuration for the RBAC system. It defines what folks can and can't do.
type AuthzSpec struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Users []*AuthzEntity `protobuf:"bytes,1,rep,name=users,proto3" json:"users,omitempty"`
	Roles []*AuthzEntity `protobuf:"bytes,2,rep,name=roles,proto3" json:"roles,omitempty"`
}

func (x *AuthzSpec) Reset() {
	*x = AuthzSpec{}
	if protoimpl.UnsafeEnabled {
		mi := &file_authz_proto_msgTypes[5]
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		ms.StoreMessageInfo(mi)
	}
}

func (x *AuthzSpec) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AuthzSpec) ProtoMessage() {}

func (x *AuthzSpec) ProtoReflect() protoreflect.Message {
	mi := &file_authz_proto_msgTypes[5]
	if protoimpl.UnsafeEnabled && x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AuthzSpec.ProtoReflect.Descriptor instead.
func (*AuthzSpec) Descriptor() ([]byte, []int) {
	return file_authz_proto_rawDescGZIP(), []int{5}
}

func (x *AuthzSpec) GetUsers() []*AuthzEntity {
	if x != nil {
		return x.Users
	}
	return nil
}

func (x *AuthzSpec) GetRoles() []*AuthzEntity {
	if x != nil {
		return x.Roles
	}
	return nil
}

var File_authz_proto protoreflect.FileDescriptor

var file_authz_proto_rawDesc = []byte{
	0x0a, 0x0b, 0x61, 0x75, 0x74, 0x68, 0x7a, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x05, 0x70,
	0x72, 0x6f, 0x74, 0x6f, 0x22, 0x1d, 0x0a, 0x05, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x12, 0x14, 0x0a,
	0x05, 0x74, 0x6f, 0x6b, 0x65, 0x6e, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x05, 0x74, 0x6f,
	0x6b, 0x65, 0x6e, 0x22, 0xbc, 0x01, 0x0a, 0x0a, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x51, 0x75, 0x65,
	0x75, 0x65, 0x12, 0x14, 0x0a, 0x05, 0x65, 0x78, 0x61, 0x63, 0x74, 0x18, 0x01, 0x20, 0x01, 0x28,
	0x09, 0x52, 0x05, 0x65, 0x78, 0x61, 0x63, 0x74, 0x12, 0x16, 0x0a, 0x06, 0x70, 0x72, 0x65, 0x66,
	0x69, 0x78, 0x18, 0x02, 0x20, 0x01, 0x28, 0x09, 0x52, 0x06, 0x70, 0x72, 0x65, 0x66, 0x69, 0x78,
	0x12, 0x32, 0x0a, 0x07, 0x61, 0x63, 0x74, 0x69, 0x6f, 0x6e, 0x73, 0x18, 0x03, 0x20, 0x03, 0x28,
	0x0e, 0x32, 0x18, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2e, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x51,
	0x75, 0x65, 0x75, 0x65, 0x2e, 0x41, 0x63, 0x74, 0x69, 0x6f, 0x6e, 0x52, 0x07, 0x61, 0x63, 0x74,
	0x69, 0x6f, 0x6e, 0x73, 0x22, 0x4c, 0x0a, 0x06, 0x41, 0x63, 0x74, 0x69, 0x6f, 0x6e, 0x12, 0x08,
	0x0a, 0x04, 0x52, 0x45, 0x41, 0x44, 0x10, 0x00, 0x12, 0x09, 0x0a, 0x05, 0x43, 0x4c, 0x41, 0x49,
	0x4d, 0x10, 0x01, 0x12, 0x0a, 0x0a, 0x06, 0x44, 0x45, 0x4c, 0x45, 0x54, 0x45, 0x10, 0x02, 0x12,
	0x0a, 0x0a, 0x06, 0x49, 0x4e, 0x53, 0x45, 0x52, 0x54, 0x10, 0x04, 0x12, 0x0a, 0x0a, 0x06, 0x43,
	0x48, 0x41, 0x4e, 0x47, 0x45, 0x10, 0x08, 0x12, 0x09, 0x0a, 0x03, 0x41, 0x4e, 0x59, 0x10, 0xff,
	0xff, 0x03, 0x22, 0x5d, 0x0a, 0x0c, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x52, 0x65, 0x71, 0x75, 0x65,
	0x73, 0x74, 0x12, 0x22, 0x0a, 0x05, 0x61, 0x75, 0x74, 0x68, 0x7a, 0x18, 0x01, 0x20, 0x01, 0x28,
	0x0b, 0x32, 0x0c, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2e, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x52,
	0x05, 0x61, 0x75, 0x74, 0x68, 0x7a, 0x12, 0x29, 0x0a, 0x06, 0x71, 0x75, 0x65, 0x75, 0x65, 0x73,
	0x18, 0x02, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x11, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2e, 0x41,
	0x75, 0x74, 0x68, 0x7a, 0x51, 0x75, 0x65, 0x75, 0x65, 0x52, 0x06, 0x71, 0x75, 0x65, 0x75, 0x65,
	0x73, 0x22, 0x4f, 0x0a, 0x0d, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e,
	0x73, 0x65, 0x12, 0x3e, 0x0a, 0x11, 0x64, 0x69, 0x73, 0x61, 0x6c, 0x6c, 0x6f, 0x77, 0x65, 0x64,
	0x5f, 0x71, 0x75, 0x65, 0x75, 0x65, 0x73, 0x18, 0x01, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x11, 0x2e,
	0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2e, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x51, 0x75, 0x65, 0x75, 0x65,
	0x52, 0x10, 0x64, 0x69, 0x73, 0x61, 0x6c, 0x6c, 0x6f, 0x77, 0x65, 0x64, 0x51, 0x75, 0x65, 0x75,
	0x65, 0x73, 0x22, 0x4c, 0x0a, 0x0b, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x45, 0x6e, 0x74, 0x69, 0x74,
	0x79, 0x12, 0x12, 0x0a, 0x04, 0x6e, 0x61, 0x6d, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52,
	0x04, 0x6e, 0x61, 0x6d, 0x65, 0x12, 0x29, 0x0a, 0x06, 0x71, 0x75, 0x65, 0x75, 0x65, 0x73, 0x18,
	0x02, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x11, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2e, 0x41, 0x75,
	0x74, 0x68, 0x7a, 0x51, 0x75, 0x65, 0x75, 0x65, 0x52, 0x06, 0x71, 0x75, 0x65, 0x75, 0x65, 0x73,
	0x22, 0x5f, 0x0a, 0x09, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x53, 0x70, 0x65, 0x63, 0x12, 0x28, 0x0a,
	0x05, 0x75, 0x73, 0x65, 0x72, 0x73, 0x18, 0x01, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x12, 0x2e, 0x70,
	0x72, 0x6f, 0x74, 0x6f, 0x2e, 0x41, 0x75, 0x74, 0x68, 0x7a, 0x45, 0x6e, 0x74, 0x69, 0x74, 0x79,
	0x52, 0x05, 0x75, 0x73, 0x65, 0x72, 0x73, 0x12, 0x28, 0x0a, 0x05, 0x72, 0x6f, 0x6c, 0x65, 0x73,
	0x18, 0x02, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x12, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x2e, 0x41,
	0x75, 0x74, 0x68, 0x7a, 0x45, 0x6e, 0x74, 0x69, 0x74, 0x79, 0x52, 0x05, 0x72, 0x6f, 0x6c, 0x65,
	0x73, 0x42, 0x09, 0x5a, 0x07, 0x2e, 0x3b, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x62, 0x06, 0x70, 0x72,
	0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_authz_proto_rawDescOnce sync.Once
	file_authz_proto_rawDescData = file_authz_proto_rawDesc
)

func file_authz_proto_rawDescGZIP() []byte {
	file_authz_proto_rawDescOnce.Do(func() {
		file_authz_proto_rawDescData = protoimpl.X.CompressGZIP(file_authz_proto_rawDescData)
	})
	return file_authz_proto_rawDescData
}

var file_authz_proto_enumTypes = make([]protoimpl.EnumInfo, 1)
var file_authz_proto_msgTypes = make([]protoimpl.MessageInfo, 6)
var file_authz_proto_goTypes = []interface{}{
	(AuthzQueue_Action)(0), // 0: proto.AuthzQueue.Action
	(*Authz)(nil),          // 1: proto.Authz
	(*AuthzQueue)(nil),     // 2: proto.AuthzQueue
	(*AuthzRequest)(nil),   // 3: proto.AuthzRequest
	(*AuthzResponse)(nil),  // 4: proto.AuthzResponse
	(*AuthzEntity)(nil),    // 5: proto.AuthzEntity
	(*AuthzSpec)(nil),      // 6: proto.AuthzSpec
}
var file_authz_proto_depIdxs = []int32{
	0, // 0: proto.AuthzQueue.actions:type_name -> proto.AuthzQueue.Action
	1, // 1: proto.AuthzRequest.authz:type_name -> proto.Authz
	2, // 2: proto.AuthzRequest.queues:type_name -> proto.AuthzQueue
	2, // 3: proto.AuthzResponse.disallowed_queues:type_name -> proto.AuthzQueue
	2, // 4: proto.AuthzEntity.queues:type_name -> proto.AuthzQueue
	5, // 5: proto.AuthzSpec.users:type_name -> proto.AuthzEntity
	5, // 6: proto.AuthzSpec.roles:type_name -> proto.AuthzEntity
	7, // [7:7] is the sub-list for method output_type
	7, // [7:7] is the sub-list for method input_type
	7, // [7:7] is the sub-list for extension type_name
	7, // [7:7] is the sub-list for extension extendee
	0, // [0:7] is the sub-list for field type_name
}

func init() { file_authz_proto_init() }
func file_authz_proto_init() {
	if File_authz_proto != nil {
		return
	}
	if !protoimpl.UnsafeEnabled {
		file_authz_proto_msgTypes[0].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*Authz); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_authz_proto_msgTypes[1].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*AuthzQueue); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_authz_proto_msgTypes[2].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*AuthzRequest); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_authz_proto_msgTypes[3].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*AuthzResponse); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_authz_proto_msgTypes[4].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*AuthzEntity); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
		file_authz_proto_msgTypes[5].Exporter = func(v interface{}, i int) interface{} {
			switch v := v.(*AuthzSpec); i {
			case 0:
				return &v.state
			case 1:
				return &v.sizeCache
			case 2:
				return &v.unknownFields
			default:
				return nil
			}
		}
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_authz_proto_rawDesc,
			NumEnums:      1,
			NumMessages:   6,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_authz_proto_goTypes,
		DependencyIndexes: file_authz_proto_depIdxs,
		EnumInfos:         file_authz_proto_enumTypes,
		MessageInfos:      file_authz_proto_msgTypes,
	}.Build()
	File_authz_proto = out.File
	file_authz_proto_rawDesc = nil
	file_authz_proto_goTypes = nil
	file_authz_proto_depIdxs = nil
}
