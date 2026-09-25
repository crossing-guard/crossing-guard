package main

import (
	"os"

	"crossing-guard/analyzers/modulekit"
	"crossing-guard/codemap"
)

var javascriptDefinition = modulekit.Definition{ModuleID: "org.crossing-guard.javascript",
	AnalyzerIdentity: analyzerIdentity, Languages: []string{"javascript", "typescript", "tsx"},
	Extensions: []string{".cjs", ".js", ".jsx", ".mjs", ".ts", ".tsx"},
	Capabilities: []codemap.Capability{codemap.CapabilityPackageDependency,
		codemap.CapabilitySymbolDeclaration, codemap.CapabilityResponsibilityFingerprint,
		codemap.CapabilitySymbolCall}}

func main() {
	os.Exit(modulekit.Main(javascriptDefinition, analyzeJavaScript))
}
