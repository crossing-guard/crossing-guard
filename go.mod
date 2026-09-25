module crossing-guard

go 1.26

require (
	github.com/ncruces/go-sqlite3 v0.35.2
	golang.org/x/image v0.45.0
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/kisielk/errcheck v1.9.0 // indirect
	github.com/ncruces/go-sqlite3-wasm/v3 v3.2.35303 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/tools v0.45.0 // indirect
	golang.org/x/tools/go/expect v0.1.1-deprecated // indirect
	honnef.co/go/tools v0.6.1 // indirect
)

tool (
	github.com/kisielk/errcheck
	honnef.co/go/tools/cmd/staticcheck
)
