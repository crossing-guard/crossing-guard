package main

import (
	"os"

	"crossing-guard/analyzers/modulekit"
	"crossing-guard/codemap"
)

var phpDefinition = modulekit.Definition{ModuleID: "org.crossing-guard.php", AnalyzerIdentity: analyzerIdentity,
	Languages: []string{"php"}, Extensions: []string{".php"}, Capabilities: []codemap.Capability{
		codemap.CapabilityPackageDependency, codemap.CapabilitySymbolDeclaration,
		codemap.CapabilityResponsibilityFingerprint, codemap.CapabilitySymbolCall}}

func main() {
	os.Exit(modulekit.Main(phpDefinition, analyzePHP))
}
