// This module has no code of its own. It exists so that every Go file under
// eval/ -- fixture sources and hidden tests -- stays out of the root module's
// `go test ./...`, `go list ./...` and golangci-lint (plan 029 §3.1.1).
module github.com/charliek/craze/eval

go 1.27.0
