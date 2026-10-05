// Command chalkfn deploys Go functions built with the chalkfn SDK to Chalk's
// external function catalog, and calls them.
//
//	chalkfn deploy [flags] [package]     build and deploy every registered function
//	chalkfn call --function NAME --input JSON
//	chalkfn run [package] --function NAME --input JSON   call a local build over gRPC
//	chalkfn functions [package]          print the package's function registry
//	chalkfn list                         list deployed external functions
//	chalkfn delete --function NAME
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/chalk-ai/chalk-go-function/internal/arrowjson"
	runtimev1 "github.com/chalk-ai/chalk-go-function/internal/gen/chalk/runtime/v1"
)

const usage = `chalkfn deploys Go functions written with github.com/chalk-ai/chalk-go-function/chalkfn.

Usage:
  chalkfn deploy [flags] [package]     build remotely and deploy every registered function
  chalkfn call --function NAME --input JSON
  chalkfn run [package] --function NAME --input JSON
                                       build locally and call a function over gRPC
  chalkfn functions [package]          print the registry of a function package
  chalkfn list                         list deployed external functions
  chalkfn delete --function NAME       delete a deployed external function

JSON inputs and outputs are column-oriented: {"x": [1, 2, 3]}.

Credentials come from CHALK_API_SERVER, CHALK_CLIENT_ID, CHALK_CLIENT_SECRET and
CHALK_ENVIRONMENT_ID, falling back to the token saved by 'chalk login'.

Run 'chalkfn <command> -h' for a command's flags.
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"deploy":    deploy,
		"call":      call,
		"run":       runLocal,
		"functions": functions,
		"list":      list,
		"delete":    deleteFn,
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		if os.Args[1] != "-h" && os.Args[1] != "--help" && os.Args[1] != "help" {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		}
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// parseFlags parses flags that may appear before or after one optional
// positional package argument, which defaults to ".".
func parseFlags(fs *flag.FlagSet, args []string) (string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	switch len(positional) {
	case 0:
		return ".", nil
	case 1:
		return positional[0], nil
	}
	return "", fmt.Errorf("expected one package, got %v", positional)
}

type envFlag map[string]string

func (e envFlag) String() string { return fmt.Sprint(map[string]string(e)) }
func (e envFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want KEY=VALUE, got %q", v)
	}
	e[k] = val
	return nil
}

func deploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	only := fs.String("function", "", "deploy only this function (default: every registered function)")
	cpu := fs.String("cpu", "", `CPU request: "250m", "500m", or a power-of-two core count`)
	mem := fs.String("memory", "", `memory request, e.g. "512Mi"`)
	gpu := fs.String("gpu", "", `GPU request, e.g. "nvidia-l4"`)
	minReplicas := fs.Int("min-replicas", 1, "minimum replicas")
	maxReplicas := fs.Int("max-replicas", 1, "maximum replicas")
	builderImage := fs.String("builder-image", "", "golang image to compile in (default: golang:<go.mod version>-bookworm)")
	runtimeImage := fs.String("runtime-image", "debian:bookworm-slim", "image the compiled binary runs in")
	cgo := fs.Bool("cgo", false, "build with CGO_ENABLED=1 (the runtime image must provide the C libraries)")
	buildTimeout := fs.Duration("build-timeout", 30*time.Minute, "how long to wait for the image build")
	readyTimeout := fs.Duration("ready-timeout", 10*time.Minute, "how long to wait for the scaling group to become ready")
	dryRun := fs.Bool("dry-run", false, "print the requests instead of sending them")
	env := envFlag{}
	fs.Var(env, "env", "KEY=VALUE environment variable for the function container (repeatable)")
	pkg, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	target, err := resolveTarget(pkg)
	if err != nil {
		return err
	}
	entries, err := registry(target)
	if err != nil {
		return err
	}
	selected := entries[:0:0]
	for _, e := range entries {
		if *only == "" || e.Name == *only {
			selected = append(selected, e)
		}
	}
	if len(selected) == 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name)
		}
		return fmt.Errorf("no functions to deploy (registered: %v)", names)
	}

	files, err := sourceFiles(target, *cgo)
	if err != nil {
		return err
	}
	source, err := tarGz(target.ModuleDir, files)
	if err != nil {
		return err
	}
	slog.Info("packaged source", "module", target.ModulePath, "package", target.buildPath(), "files", len(files), "bytes", len(source))
	if len(source) > maxInlineSourceBytes {
		return fmt.Errorf(
			"compressed source is %d bytes; Chalk image builds accept at most %d bytes of inline source. Move code the function does not import out of its package graph, or split it into a published module",
			len(source), maxInlineSourceBytes)
	}
	if *builderImage == "" {
		*builderImage = defaultBuilderImage(target.GoVersion)
	}
	spec := imageSpec(target, source, imageOptions{BuilderImage: *builderImage, RuntimeImage: *runtimeImage, CGO: *cgo})

	requests := make([]map[string]any, 0, len(selected))
	for _, e := range selected {
		requests = append(requests, map[string]any{
			"functionName":      e.Name,
			"inputArrowSchema":  e.InputSchema,
			"outputArrowSchema": e.OutputSchema,
			"spec": map[string]any{
				"containerSpec": containerSpec(dnsLabel(e.Name), "", containerOptions{CPU: *cpu, Memory: *mem, GPU: *gpu, Env: env}),
				"scalingSpec":   map[string]any{"minReplicas": *minReplicas, "maxReplicas": *maxReplicas},
			},
		})
	}

	if *dryRun {
		return printJSON(map[string]any{
			"files":                          files,
			"getOrBuildCustomImage":          map[string]any{"imageSpec": spec},
			"createExternalFunctionVersions": requests,
		})
	}

	client, err := newAPIClient()
	if err != nil {
		return err
	}
	slog.Info("requesting image", "builder", *builderImage, "runtime", *runtimeImage)
	image, cached, err := client.getOrBuildImage(spec, *buildTimeout)
	if err != nil {
		return err
	}
	slog.Info("image ready", "image", image, "cached", cached)

	for _, req := range requests {
		cs := req["spec"].(map[string]any)["containerSpec"].(map[string]any)
		cs["image"] = image
		name := req["functionName"].(string)
		// A function stays bound to the scaling group of its first version, and
		// a version with a different container name is rejected.
		existing, err := client.currentScalingGroup(name)
		if err != nil {
			return err
		}
		if existing != "" {
			cs["name"] = existing
		}
		var resp struct {
			ExternalFunctionVersion struct {
				ID               string `json:"id"`
				Version          int    `json:"version"`
				ScalingGroupName string `json:"scalingGroupName"`
			} `json:"externalFunctionVersion"`
			ScalingGroup struct {
				Name string `json:"name"`
			} `json:"scalingGroup"`
		}
		slog.Info("registering function", "function", name)
		if err := client.rpc(catalogService, "CreateExternalFunctionVersion", req, &resp, 2*time.Minute); err != nil {
			return err
		}
		efv := resp.ExternalFunctionVersion
		sg := firstNonEmpty(resp.ScalingGroup.Name, efv.ScalingGroupName, cs["name"].(string))
		slog.Info("created version", "function", name, "version", efv.Version, "id", efv.ID, "scaling_group", sg)
		if *minReplicas == 0 {
			continue
		}
		if err := client.waitScalingGroupReady(sg, *readyTimeout); err != nil {
			return err
		}
		inSchema, err := arrowjson.ToArrowSchema(req["inputArrowSchema"].(json.RawMessage))
		if err != nil {
			return err
		}
		empty, err := emptyIPC(inSchema)
		if err != nil {
			return err
		}
		if err := client.waitCallable(name, empty, *readyTimeout); err != nil {
			return fmt.Errorf("probing %s with an empty batch: %w", name, err)
		}
		slog.Info("function is serving", "function", name, "scaling_group", sg)
	}
	return nil
}

// registry builds the target locally and returns the functions it registers.
func registry(t buildTarget) ([]registryEntry, error) {
	dir, err := os.MkdirTemp("", "chalkfn-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	bin, err := buildLocal(t, dir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "list-functions")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s list-functions: %w (does main call chalkfn.Serve()?)", t.buildPath(), err)
	}
	var entries []registryEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parsing list-functions output: %w", err)
	}
	return entries, nil
}

type registryEntry struct {
	Name         string          `json:"name"`
	InputSchema  json.RawMessage `json:"inputArrowSchema"`
	OutputSchema json.RawMessage `json:"outputArrowSchema"`
}

func functions(args []string) error {
	fs := flag.NewFlagSet("functions", flag.ContinueOnError)
	pkg, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	t, err := resolveTarget(pkg)
	if err != nil {
		return err
	}
	entries, err := registry(t)
	if err != nil {
		return err
	}
	return printJSON(entries)
}

func call(args []string) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	name := fs.String("function", "", "function to call (required)")
	input := fs.String("input", "", `column-oriented JSON input, e.g. {"x":[1,2,3]} (required)`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *input == "" {
		return errors.New("--function and --input are required")
	}
	client, err := newAPIClient()
	if err != nil {
		return err
	}
	var efv struct {
		ExternalFunctionVersion struct {
			InputArrowSchema json.RawMessage `json:"inputArrowSchema"`
		} `json:"externalFunctionVersion"`
	}
	if err := client.rpc(catalogService, "GetExternalFunctionVersion",
		map[string]any{"key": map[string]any{"functionName": *name}}, &efv, 30*time.Second); err != nil {
		return err
	}
	schema, err := arrowjson.ToArrowSchema(efv.ExternalFunctionVersion.InputArrowSchema)
	if err != nil {
		return err
	}
	payload, err := columnsToIPC(*input, schema)
	if err != nil {
		return err
	}
	var resp struct {
		RemoteCallResponse struct {
			FeatherStream string `json:"featherStream"`
		} `json:"remoteCallResponse"`
	}
	err = client.rpc(catalogService, "CallExternalFunction", map[string]any{
		"function":          map[string]any{"functionName": *name},
		"remoteCallRequest": map[string]any{"name": *name, "featherStream": payload},
	}, &resp, 5*time.Minute)
	if err != nil {
		return err
	}
	out, err := base64.StdEncoding.DecodeString(resp.RemoteCallResponse.FeatherStream)
	if err != nil {
		return fmt.Errorf("decoding featherStream: %w", err)
	}
	text, err := ipcToColumns(out)
	if err != nil {
		return err
	}
	fmt.Println(text)
	return nil
}

// runLocal builds a function package for the host, serves it on a loopback
// port, and calls one function through the same gRPC contract Chalk uses.
func runLocal(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	name := fs.String("function", "", "function to call (required)")
	input := fs.String("input", "", `column-oriented JSON input, e.g. {"x":[1,2,3]} (required)`)
	pkg, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if *name == "" || *input == "" {
		return errors.New("--function and --input are required")
	}
	t, err := resolveTarget(pkg)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "chalkfn-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	bin, err := buildLocal(t, dir)
	if err != nil {
		return err
	}
	out, err := exec.Command(bin, "list-functions").Output()
	if err != nil {
		return fmt.Errorf("list-functions: %w", err)
	}
	var entries []registryEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return err
	}
	var entry *registryEntry
	for i := range entries {
		if entries[i].Name == *name {
			entry = &entries[i]
		}
	}
	if entry == nil {
		return fmt.Errorf("function %q is not registered by %s", *name, pkg)
	}
	schema, err := arrowjson.ToArrowSchema(entry.InputSchema)
	if err != nil {
		return err
	}
	payload, err := columnsToIPC(*input, schema)
	if err != nil {
		return err
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	server := exec.Command(bin, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(port))
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		return err
	}
	defer func() {
		_ = server.Process.Signal(os.Interrupt)
		_ = server.Wait()
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := waitForPort(ctx, addr); err != nil {
		return err
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<30), grpc.MaxCallSendMsgSize(1<<30)))
	if err != nil {
		return err
	}
	defer conn.Close()
	stream, err := runtimev1.NewRemoteCallServiceClient(conn).CallFunction(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&runtimev1.CallFunctionRequest{Name: *name, FeatherStream: payload}); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	resp, err := stream.Recv()
	if err != nil {
		return err
	}
	text, err := ipcToColumns(resp.GetFeatherStream())
	if err != nil {
		return err
	}
	fmt.Println(text)
	return nil
}

func list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := newAPIClient()
	if err != nil {
		return err
	}
	var resp json.RawMessage
	if err := client.rpc(catalogService, "ListExternalFunctions", map[string]any{}, &resp, time.Minute); err != nil {
		return err
	}
	return printJSON(resp)
}

func deleteFn(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	name := fs.String("function", "", "function to delete (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--function is required")
	}
	client, err := newAPIClient()
	if err != nil {
		return err
	}
	if err := client.rpc(catalogService, "DeleteExternalFunction", map[string]any{"functionName": *name}, nil, time.Minute); err != nil {
		return err
	}
	slog.Info("deleted function", "function", *name)
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitForPort(ctx context.Context, addr string) error {
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("server on %s did not start: %w", addr, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
