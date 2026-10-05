// Package gen holds protobuf stubs generated from ../../proto.
package gen

//go:generate protoc --proto_path=../../proto --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative chalk/runtime/v1/remote_python_call.proto
