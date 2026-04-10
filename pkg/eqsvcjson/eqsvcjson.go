// Package eqsvcjson provides an HTTP handler that wraps the gRPC eqsvcgrpc.QSvc
// to serve EntroQ requests over JSON and gRPC-Web using ConnectRPC.
package eqsvcjson

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/api/apiconnect"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"google.golang.org/grpc/metadata"
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

	// Add our custom splicing codec to the options.
	opts = append(opts, connect.WithCodec(newSplicingCodec()))

	connectPath, connectHandler := apiconnect.NewEntroQHandler(h, opts...)

	services := []*vanguard.Service{
		vanguard.NewService(apiconnect.EntroQName, connectHandler),
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
