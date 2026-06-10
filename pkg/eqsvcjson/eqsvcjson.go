// Package eqsvcjson provides an HTTP handler that wraps the gRPC eqsvcgrpc.QSvc
// to serve EntroQ requests over JSON and gRPC-Web using ConnectRPC.
package eqsvcjson

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/api/apiconnect"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Handler implements protoconnect.EntroQHandler by wrapping eqsvcgrpc.QSvc.
// Handler implements apiconnect.EntroQHandler by wrapping eqsvcgrpc.QSvc.
type Handler struct {
	svc *eqsvcgrpc.QSvc
}

// New creates a new HTTP handler for the EntroQ JSON/Connect endpoints.
// It uses Vanguard to provide RESTful transcoding under /api/v0.
func New(svc *eqsvcgrpc.QSvc, opts ...connect.HandlerOption) (string, http.Handler, error) {
	h := &Handler{svc: svc}

	// QSvc returns google.golang.org/grpc/status errors, which ConnectRPC does
	// not recognize — left untranslated they collapse to CodeUnknown (HTTP 500)
	// with their status code stringified into the message and their details
	// dropped. Translate them centrally so codes and details survive over JSON.
	opts = append(opts, connect.WithInterceptors(errTranslator()))

	connectPath, connectHandler := apiconnect.NewEntroQHandler(h, opts...)

	services := []*vanguard.Service{
		// Talk to the in-process Connect backend in proto, not JSON. Otherwise
		// Vanguard relays the backend's JSON bytes verbatim, and connect-go's
		// codec omits zero-valued fields (e.g. version:0, atMs:0) — leaving
		// thin clients to guess at missing fields. Forcing proto makes Vanguard
		// re-marshal REST responses with its own EmitUnpopulated codec, so zero
		// values are emitted explicitly.
		vanguard.NewService(apiconnect.EntroQName, connectHandler,
			vanguard.WithTargetProtocols(vanguard.ProtocolGRPC),
			vanguard.WithTargetCodecs(vanguard.CodecProto),
		),
	}

	transcoder, err := vanguard.NewTranscoder(services)
	if err != nil {
		return "", nil, err
	}

	mux := http.NewServeMux()
	mux.Handle(connectPath, connectHandler)
	mux.Handle("/", transcoder)

	return "/", mux, nil
}

func ctxWithMD(ctx context.Context, headers http.Header) context.Context {
	md := metadata.MD{}
	for k, v := range headers {
		md[strings.ToLower(k)] = v
	}
	return metadata.NewIncomingContext(ctx, md)
}

// errTranslator returns a unary interceptor that converts grpc/status errors
// from the wrapped QSvc into ConnectRPC errors, preserving codes and details.
func errTranslator() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resp, err := next(ctx, req)
			if err != nil {
				return resp, translateErr(err)
			}
			return resp, nil
		}
	}
}

// translateErr converts a google.golang.org/grpc/status error into a
// *connect.Error. gRPC and Connect codes share numeric values, so the code maps
// directly — except a dependency error (NotFound carrying ModifyDep details),
// which becomes Aborted (HTTP 409 Conflict): it is an optimistic-concurrency
// failure the caller should retry with fresh versions, not a "resource missing"
// 404, which is also cacheable. The ModifyDep/AuthzDep details are re-attached
// so JSON clients can see which items conflicted.
func translateErr(err error) error {
	stat, ok := status.FromError(err)
	if !ok {
		return err // not a grpc status; let Connect handle it.
	}
	code := connect.Code(stat.Code())
	var details []proto.Message
	isDep := false
	for _, d := range stat.Details() {
		switch m := d.(type) {
		case *pb.ModifyDep:
			isDep = true
			details = append(details, m)
		case *pb.AuthzDep:
			details = append(details, m)
		}
	}
	if isDep {
		// TODO(next-minor): emit codes.Aborted from eqsvcgrpc directly and drop
		// this remap; the eqgrpc client already accepts both NotFound and
		// Aborted, so the server can switch without breaking older clients.
		code = connect.CodeAborted
	}
	cerr := connect.NewError(code, errors.New(stat.Message()))
	for _, d := range details {
		if detail, derr := connect.NewErrorDetail(d); derr == nil {
			cerr.AddDetail(detail)
		}
	}
	return cerr
}

func (h *Handler) TryClaim(ctx context.Context, req *connect.Request[pb.ClaimRequest]) (*connect.Response[pb.ClaimResponse], error) {
	resp, err := h.svc.TryClaim(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) Claim(ctx context.Context, req *connect.Request[pb.ClaimRequest]) (*connect.Response[pb.ClaimResponse], error) {
	resp, err := h.svc.Claim(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) Modify(ctx context.Context, req *connect.Request[pb.ModifyRequest]) (*connect.Response[pb.ModifyResponse], error) {
	resp, err := h.svc.Modify(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) Tasks(ctx context.Context, req *connect.Request[pb.TasksRequest]) (*connect.Response[pb.TasksResponse], error) {
	resp, err := h.svc.Tasks(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) Queues(ctx context.Context, req *connect.Request[pb.QueuesRequest]) (*connect.Response[pb.QueuesResponse], error) {
	resp, err := h.svc.Queues(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) QueueStats(ctx context.Context, req *connect.Request[pb.QueuesRequest]) (*connect.Response[pb.QueuesResponse], error) {
	resp, err := h.svc.QueueStats(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) Time(ctx context.Context, req *connect.Request[pb.TimeRequest]) (*connect.Response[pb.TimeResponse], error) {
	resp, err := h.svc.Time(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) Docs(ctx context.Context, req *connect.Request[pb.DocsRequest]) (*connect.Response[pb.DocsResponse], error) {
	resp, err := h.svc.Docs(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) ClaimDocs(ctx context.Context, req *connect.Request[pb.ClaimDocsRequest]) (*connect.Response[pb.ClaimDocsResponse], error) {
	resp, err := h.svc.ClaimDocs(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (h *Handler) NamespaceStats(ctx context.Context, req *connect.Request[pb.NamespacesRequest]) (*connect.Response[pb.NamespacesResponse], error) {
	resp, err := h.svc.NamespaceStats(ctxWithMD(ctx, req.Header()), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// streamAdapter wraps a Connect server stream so it implements the grpc.ServerStream expected by eqsvcgrpc.
type streamAdapter struct {
	ctx    context.Context
	stream *connect.ServerStream[pb.TasksResponse]
	pb.EntroQ_StreamTasksServer
}

func (s *streamAdapter) Context() context.Context {
	return s.ctx
}

func (s *streamAdapter) Send(msg *pb.TasksResponse) error {
	return s.stream.Send(msg)
}

func (h *Handler) StreamTasks(ctx context.Context, req *connect.Request[pb.TasksRequest], stream *connect.ServerStream[pb.TasksResponse]) error {
	adapter := &streamAdapter{
		ctx:    ctxWithMD(ctx, req.Header()),
		stream: stream,
	}
	return h.svc.StreamTasks(req.Msg, adapter)
}
