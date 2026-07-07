package nexus

// temporal/api/common/v1/message.proto is a vendored, hand-maintained subset of the
// upstream go.temporal.io/api common message definitions, checked in only so that
// `protoc --proto_path=.` can resolve the Payload import. It must stay a faithful
// subset of the upstream definition; update it if the upstream Payload message changes.

//go:generate protoc --proto_path=. --go_out=. --go_opt=paths=source_relative nexus.proto nexus_invoke.proto nexus_worker_set.proto
//go:generate npx --yes nexus-rpc-gen@0.1.0-alpha.4 --lang go --package nexus --out-file nexus_invoke_nexusrpc_gen.go nexus_invoke.nexusrpc.yaml
//go:generate npx --yes nexus-rpc-gen@0.1.0-alpha.4 --lang go --package nexus --out-file nexus_worker_set_nexusrpc_gen.go nexus_worker_set.nexusrpc.yaml
//go:generate go run ./internal/nexusservergen --package nexus --out-file nexus_invoke_nexusserver_gen.go nexus_invoke.nexusrpc.yaml
//go:generate go run ./internal/nexusservergen --package nexus --out-file nexus_worker_set_nexusserver_gen.go nexus_worker_set.nexusrpc.yaml
