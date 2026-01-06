#!/usr/bin/env bash
set -euo pipefail
CFG=${1:-./configs/config.local.json}
go run ./cmd/bot -config "$CFG"
