#!/usr/bin/env bash

set -eo pipefail

GOOS=linux CGO_ENABLED=0 go build -ldflags "-w" -o gpumon main.go

