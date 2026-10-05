package main

import (
	"fmt"
	"regexp"
	"strings"
)

// maxInlineSourceBytes is the custom image service's cap on inline file
// content per image spec (go-api-server compute/imagebuilder.ValidateImageSpec).
const maxInlineSourceBytes = 32 * 1024

const (
	serverPath = "/app/server"
	serverPort = 6666
)

type imageOptions struct {
	BuilderImage string // golang image the source is compiled in
	RuntimeImage string // image the compiled binary runs in
	CGO          bool
}

// defaultBuilderImage picks the official golang image matching the go.mod
// `go` directive; GOTOOLCHAIN=auto covers patch releases newer than the image.
func defaultBuilderImage(goVersion string) string {
	m := regexp.MustCompile(`^(\d+)\.(\d+)`).FindString(goVersion)
	if m == "" {
		m = "1"
	}
	return fmt.Sprintf("golang:%s-bookworm", m)
}

// imageSpec is the JSON form of chalk.sandbox.v1.ImageSpec. It is a two-stage
// build: the base stage unpacks the source and compiles it, then the
// dockerfile step starts a runtime stage holding only the binary and CA
// certificates. The spec-level entrypoint is emitted after every step, so it
// applies to the runtime stage.
func imageSpec(t buildTarget, source []byte, opts imageOptions) map[string]any {
	build := fmt.Sprintf(
		"CGO_ENABLED=%s GOFLAGS=-mod=mod GOWORK=off go build -trimpath -ldflags='-s -w' -o /out/server %s",
		boolToBit(opts.CGO), t.buildPath(),
	)
	return map[string]any{
		"baseImage": opts.BuilderImage,
		"workdir":   "/src",
		"steps": []any{
			map[string]any{"addFile": map[string]any{
				"destination": "/src/source.tar.gz",
				"content":     source, // []byte marshals as base64, the protobuf-JSON bytes encoding
			}},
			map[string]any{"runCommands": map[string]any{"commands": []string{
				"tar xzf source.tar.gz && rm source.tar.gz",
				build,
			}}},
			map[string]any{"dockerfileCommands": map[string]any{"commands": []string{
				"FROM " + opts.RuntimeImage,
				"COPY --from=0 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
				"COPY --from=0 /out/server " + serverPath,
			}}},
		},
		"entrypoint": []string{serverPath},
	}
}

type containerOptions struct {
	CPU, Memory, GPU string
	Env              map[string]string
}

// containerSpec is the JSON form of chalk.container.v1.ChalkContainerSpec.
func containerSpec(name, image string, opts containerOptions) map[string]any {
	spec := map[string]any{
		"name":       name,
		"image":      image,
		"port":       serverPort,
		"protocol":   "grpc",
		"entrypoint": []string{serverPath},
	}
	if len(opts.Env) > 0 {
		spec["envVars"] = opts.Env
	}
	resources := map[string]any{}
	for k, v := range map[string]string{"cpu": opts.CPU, "memory": opts.Memory, "gpu": opts.GPU} {
		if v != "" {
			resources[k] = v
		}
	}
	if len(resources) > 0 {
		spec["resources"] = resources
	}
	return spec
}

// dnsLabel turns s into a valid Kubernetes DNS label.
func dnsLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 60 {
		out = strings.Trim(out[:60], "-")
	}
	return out
}
