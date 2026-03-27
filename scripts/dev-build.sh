#!/bin/sh
set -e

# Build web
cd web && pnpm build && cd ..

# Copy web output to embed dir
rm -rf internal/setup/web
cp -r web/out internal/setup/web

# Build Go binary
go build -ldflags "-X main.version=dev" -o tmp/fastclaw ./cmd/fastclaw
