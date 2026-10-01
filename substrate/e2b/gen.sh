#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
P=github.com/jrimmer/spoond/substrate/e2b/gen
rm -rf gen && mkdir -p gen/orchestrator gen/info gen/template gen/envd/process gen/envd/filesystem
M="--go_opt=Morchestrator.proto=$P/orchestrator --go-grpc_opt=Morchestrator.proto=$P/orchestrator \
   --go_opt=Minfo.proto=$P/info --go-grpc_opt=Minfo.proto=$P/info \
   --go_opt=Mtemplate-manager.proto=$P/template --go-grpc_opt=Mtemplate-manager.proto=$P/template"
protoc -I proto --go_out=gen/orchestrator --go_opt=paths=source_relative --go-grpc_out=gen/orchestrator --go-grpc_opt=paths=source_relative $M proto/orchestrator.proto
protoc -I proto --go_out=gen/info --go_opt=paths=source_relative --go-grpc_out=gen/info --go-grpc_opt=paths=source_relative $M proto/info.proto
protoc -I proto --go_out=gen/template --go_opt=paths=source_relative --go-grpc_out=gen/template --go-grpc_opt=paths=source_relative $M proto/template-manager.proto
protoc -I proto/envd --go_out=gen/envd --go_opt=paths=source_relative --go_opt=Mprocess/process.proto=$P/envd/process --connect-go_out=gen/envd --connect-go_opt=paths=source_relative --connect-go_opt=Mprocess/process.proto=$P/envd/process process/process.proto
protoc -I proto/envd --go_out=gen/envd --go_opt=paths=source_relative --go_opt=Mfilesystem/filesystem.proto=$P/envd/filesystem --connect-go_out=gen/envd --connect-go_opt=paths=source_relative --connect-go_opt=Mfilesystem/filesystem.proto=$P/envd/filesystem filesystem/filesystem.proto
gofmt -w gen
