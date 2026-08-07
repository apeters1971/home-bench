.PHONY: all controller client

all: controller client

controller:
	go build -o bin/homebench-controller ./cmd/controller

client:
	go build -o bin/homebench-client ./cmd/client
