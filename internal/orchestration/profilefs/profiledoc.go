package profilefs

import (
	"time"

	"gopkg.in/yaml.v3"

	"crossing-guard/profiledoc"
)

// The portable PROFILE.md parser lives in the top-level profiledoc package so the
// bundle build tool, the team server (through its client pin), and this device all
// parse one way (team rest-of-release plan §4.1 decision 2). profilefs keeps
// storage; these aliases keep every existing caller's names unchanged.
const (
	FormatVersion  = profiledoc.FormatVersion
	ProfileKind    = profiledoc.ProfileKind
	MaxSourceBytes = profiledoc.MaxSourceBytes
)

type (
	Problem              = profiledoc.Problem
	CompiledProfile      = profiledoc.CompiledProfile
	CompiledTrigger      = profiledoc.CompiledTrigger
	CompiledContext      = profiledoc.CompiledContext
	CompiledOutput       = profiledoc.CompiledOutput
	CompiledRequirements = profiledoc.CompiledRequirements
	CompiledDestination  = profiledoc.CompiledDestination
	CompiledLimits       = profiledoc.CompiledLimits
	CompiledFailure      = profiledoc.CompiledFailure
	CompiledPresentation = profiledoc.CompiledPresentation
	Document             = profiledoc.Document
	Preview              = profiledoc.Preview
)

// Parse parses one PROFILE.md source through the shared parser.
func Parse(sourceName string, source []byte) (Document, error) {
	return profiledoc.Parse(sourceName, source)
}

// ProblemCode returns err's bounded problem code, or "" when err is not a Problem.
func ProblemCode(err error) string { return profiledoc.ProblemCode(err) }

// Duration parses a profile duration scalar.
func Duration(value string) (time.Duration, error) { return profiledoc.Duration(value) }

func problem(code, field, message, recovery string) error {
	return profiledoc.NewProblem(code, field, message, recovery)
}

func storageProblem(cause error) error { return profiledoc.StorageProblem(cause) }

func invalid(field, recovery string) error { return profiledoc.Invalid(field, recovery) }

func framedDigest(frame string, body []byte) string { return profiledoc.FramedDigest(frame, body) }

var profileIDPattern = profiledoc.ProfileIDPattern

func validSemver(value string) bool { return profiledoc.ValidSemver(value) }

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if candidate == value {
			return true
		}
	}
	return false
}

func splitProfile(source []byte) ([]byte, []byte, error) { return profiledoc.SplitProfile(source) }

func decodeYAMLNode(frontmatter []byte) (*yaml.Node, error) {
	return profiledoc.DecodeYAMLNode(frontmatter)
}

func mappingValue(mapping *yaml.Node, name string) *yaml.Node {
	return profiledoc.MappingValue(mapping, name)
}
