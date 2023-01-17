#!/usr/bin/env bash

set -e

echo "Building tap-cni plugins"
go build --mod=vendor -o ./bin/tap ./tap/
