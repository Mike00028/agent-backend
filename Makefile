PROTO_SRC   := proto/langgraph/v1/agent.proto
GO_OUT      := golang/services/api/internal/langgraphv1
PYTHON_OUT  := python/gen

.PHONY: proto run-go run-python test-go test-python lint help

## Generate protobuf stubs for Go and Python
proto:
	@echo "── Generating Go stubs ──"
	@if not exist "$(GO_OUT)" mkdir "$(GO_OUT)"
	protoc --proto_path=proto --go_out="$(GO_OUT)" --go_opt=paths=source_relative --go-grpc_out="$(GO_OUT)" --go-grpc_opt=paths=source_relative $(PROTO_SRC)
	@echo "── Generating Python stubs ──"
	@if not exist "$(PYTHON_OUT)" mkdir "$(PYTHON_OUT)"
	cd python && poetry run python -m grpc_tools.protoc --proto_path=../proto --python_out=gen --pyi_out=gen --grpc_python_out=gen ../$(PROTO_SRC)
	@echo "done."

## Run the Go API gateway
run-go:
	cd golang/services/api && go run ./cmd

## Install Python dependencies via Poetry
install-python:
	cd python && poetry install

## Run the Python LangGraph gRPC server
run-python:
	cd python && poetry run python server.py

## Run Go tests across all modules
test-go:
	cd golang/services/api && go test ./...

## Run Python tests
test-python:
	cd python && poetry run pytest

## Lint both stacks
lint:
	cd golang/services/api && golangci-lint run ./...
	cd python && poetry run ruff check .

help:
	@grep -E '^##' Makefile | sed 's/## //'
