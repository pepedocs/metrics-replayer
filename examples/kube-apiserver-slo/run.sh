#!/usr/bin/env bash
# Replay the test matrix in scenario.yaml and report how each alert performed.
# Usage: make clean && make up && ./examples/kube-apiserver-slo/run.sh [--markdown]
set -euo pipefail
cd "$(dirname "$0")/../.."
go run ./cmd/emit --scenario examples/kube-apiserver-slo/scenario.yaml --report "$@"
