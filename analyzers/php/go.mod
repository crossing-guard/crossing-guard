module crossing-guard-analyzer-php

go 1.26.0

require (
	crossing-guard v0.0.0
	github.com/tree-sitter/go-tree-sitter v0.25.0
	github.com/tree-sitter/tree-sitter-php v0.24.2
)

require github.com/mattn/go-pointer v0.0.1 // indirect

replace crossing-guard => ../..
