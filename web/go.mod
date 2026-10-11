// web/ holds the TypeScript UI, not Go. This file only draws a module
// boundary so the root module's ./... never walks into web/node_modules:
// npm packages can ship .go files (flatted does), and without it a local
// `go build ./...`, `make lint` or `make cover` after `make ui` compiles,
// lints and counts them.
module github.com/daiwa-zou/kiln/web

go 1.26.0
