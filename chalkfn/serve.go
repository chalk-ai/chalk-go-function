package chalkfn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/chalk-ai/chalk-go-function/internal/arrowjson"
	runtimev1 "github.com/chalk-ai/chalk-go-function/internal/gen/chalk/runtime/v1"
)

// DefaultPort matches the port Chalk's scaling groups route external function
// calls to.
const DefaultPort = 6666

// Serve runs the command named by os.Args and exits the process when it is
// done. Call it from main after registering functions.
//
// Commands:
//
//	serve           run the gRPC server (default)
//	list-functions  print the registry as JSON; the chalkfn CLI reads this to
//	                learn each function's schemas before deploying
func Serve() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "list-functions":
		return writeRegistryJSON(os.Stdout)
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		host := fs.String("host", envOr("CHALK_REMOTE_CALL_HOST", "0.0.0.0"), "bind address")
		port := fs.Int("port", envInt("CHALK_REMOTE_CALL_PORT", DefaultPort), "listen port")
		if err := fs.Parse(args); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return ListenAndServe(ctx, net.JoinHostPort(*host, fmt.Sprint(*port)))
	default:
		return fmt.Errorf("unknown command %q (want serve or list-functions)", cmd)
	}
}

// RegistryEntry describes one registered function in the form the external
// function catalog expects.
type RegistryEntry struct {
	Name         string           `json:"name"`
	InputSchema  arrowjson.Schema `json:"inputArrowSchema"`
	OutputSchema arrowjson.Schema `json:"outputArrowSchema"`
}

func writeRegistryJSON(w io.Writer) error {
	entries := []RegistryEntry{}
	for _, f := range Functions() {
		in, err := arrowjson.FromArrowSchema(f.Input)
		if err != nil {
			return fmt.Errorf("function %q input: %w", f.Name, err)
		}
		out, err := arrowjson.FromArrowSchema(f.Output)
		if err != nil {
			return fmt.Errorf("function %q output: %w", f.Name, err)
		}
		entries = append(entries, RegistryEntry{Name: f.Name, InputSchema: in, OutputSchema: out})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(entries)
}

// ListenAndServe serves every registered function over
// chalk.runtime.v1.RemoteCallService on addr until ctx is cancelled.
func ListenAndServe(ctx context.Context, addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := NewGRPCServer()
	names := []string{}
	for _, f := range Functions() {
		names = append(names, f.Name)
	}
	slog.Info("serving chalk functions", "addr", lis.Addr().String(), "functions", names)

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// NewGRPCServer returns a gRPC server with the remote call service and the
// standard health service registered, for callers that manage their own
// listener.
func NewGRPCServer(opts ...grpc.ServerOption) *grpc.Server {
	opts = append([]grpc.ServerOption{
		grpc.MaxRecvMsgSize(1 << 30),
		grpc.MaxSendMsgSize(1 << 30),
	}, opts...)
	srv := grpc.NewServer(opts...)
	runtimev1.RegisterRemoteCallServiceServer(srv, &remoteCallServer{mem: memory.NewGoAllocator()})
	healthpb.RegisterHealthServer(srv, health.NewServer())
	return srv
}

type remoteCallServer struct {
	runtimev1.UnimplementedRemoteCallServiceServer
	mem memory.Allocator
}

// CallFunction answers each request on the stream with one response, in order.
func (s *remoteCallServer) CallFunction(stream grpc.BidiStreamingServer[runtimev1.CallFunctionRequest, runtimev1.CallFunctionResponse]) error {
	ctx := stream.Context()
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		out, err := s.call(ctx, req.GetName(), req.GetFeatherStream())
		if err != nil {
			slog.Error("function call failed", "function", req.GetName(), "error", err)
			return err
		}
		if err := stream.Send(&runtimev1.CallFunctionResponse{FeatherStream: out}); err != nil {
			return err
		}
	}
}

func (s *remoteCallServer) call(ctx context.Context, name string, payload []byte) ([]byte, error) {
	f, ok := Lookup(name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "function %q is not registered", name)
	}
	return Invoke(ctx, s.mem, f, payload)
}

// Invoke runs f over every record batch in an Arrow IPC stream and returns the
// results as an Arrow IPC stream. It is what the server runs per request, and
// is exported so functions can be tested without a network round trip.
func Invoke(ctx context.Context, mem memory.Allocator, f Function, payload []byte) (_ []byte, err error) {
	reader, err := ipc.NewReader(bytes.NewReader(payload), ipc.WithAllocator(mem))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "reading input arrow stream: %v", err)
	}
	defer reader.Release()
	if got, want := reader.Schema().NumFields(), f.Input.NumFields(); got != want {
		return nil, status.Errorf(codes.InvalidArgument, "function %q takes %d columns, got %d", f.Name, want, got)
	}

	var buf bytes.Buffer
	writer := ipc.NewWriter(&buf, ipc.WithSchema(f.Output), ipc.WithAllocator(mem))
	for reader.Next() {
		if err := writeResult(ctx, mem, f, reader.RecordBatch(), writer); err != nil {
			return nil, err
		}
	}
	if err := reader.Err(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "reading input arrow stream: %v", err)
	}
	if err := writer.Close(); err != nil {
		return nil, status.Errorf(codes.Internal, "finishing output arrow stream: %v", err)
	}
	return buf.Bytes(), nil
}

func writeResult(ctx context.Context, mem memory.Allocator, f Function, in arrow.RecordBatch, w *ipc.Writer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "function %q panicked: %v", f.Name, r)
		}
	}()
	out, err := f.Fn(ctx, mem, in)
	if err != nil {
		if _, isStatus := status.FromError(err); isStatus {
			return err
		}
		return status.Errorf(codes.Internal, "function %q: %v", f.Name, err)
	}
	defer out.Release()
	if err := checkOutput(f, in, out); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	// The writer's schema is the declared Output schema; rebuild the record
	// over it so column names a function chose ad hoc never leak to callers.
	rec := arrowRecord(f.Output, out)
	defer rec.Release()
	return w.Write(rec)
}

func checkOutput(f Function, in, out arrow.RecordBatch) error {
	if out.NumRows() != in.NumRows() {
		return fmt.Errorf("function %q returned %d rows for %d input rows", f.Name, out.NumRows(), in.NumRows())
	}
	if got, want := int(out.NumCols()), f.Output.NumFields(); got != want {
		return fmt.Errorf("function %q returned %d columns, declared %d", f.Name, got, want)
	}
	for i, field := range f.Output.Fields() {
		if got := out.Column(i).DataType(); !arrow.TypeEqual(got, field.Type) {
			return fmt.Errorf("function %q output column %d (%q) has type %s, declared %s", f.Name, i, field.Name, got, field.Type)
		}
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	var n int
	if _, err := fmt.Sscan(os.Getenv(key), &n); err == nil {
		return n
	}
	return def
}
