// Package profiledoc is the portable PROFILE.md format: parse, compile, and digest.
//
// It is a top-level package with no dependency on any internal package so that three
// places run one parser: the lead's bundle build tool, the team server's publish route
// (which imports this module at its client pin), and each device at adoption (team
// rest-of-release plan §4.1 decision 2). Storage, selections, drafts, and edits stay in
// internal/orchestration/profilefs, which imports this package and aliases its names.
// The package deliberately has no runtime, task, approval, model, event, or binding
// behavior; the published signal set and the context-selection type live here because
// a profile's trigger and context are validated against them.
package profiledoc

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	FormatVersion = 1
	ProfileKind   = "crossing-guard-orchestration-profile"

	MaxSourceBytes      = 262_144
	maxFrontmatterBytes = 65_536
	maxInstructionBytes = 196_608
	maxContextBytes     = 262_144
)

var (
	// ProfileIDPattern is the authored profile id grammar.
	ProfileIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	stableIDPattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	durationPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)(ms|s|m)$`)
)

// Problem is a bounded domain error. Message and Recovery are fixed product copy;
// Field is a schema path. None may contain authored scalar or instruction bytes.
type Problem struct {
	Code     string `json:"code"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
	Recovery string `json:"recovery,omitempty"`
	cause    error
}

func (p *Problem) Error() string {
	if p.Field == "" {
		return p.Message
	}
	return p.Field + ": " + p.Message
}

func (p *Problem) Unwrap() error { return p.cause }

// NewProblem builds a bounded domain error. Callers pass fixed product copy only.
func NewProblem(code, field, message, recovery string) error {
	return &Problem{Code: code, Field: field, Message: message, Recovery: recovery}
}

func problem(code, field, message, recovery string) error {
	return NewProblem(code, field, message, recovery)
}

// WrapProblem builds a bounded domain error that keeps its cause reachable through
// Unwrap. The cause is never rendered.
func WrapProblem(code, message, recovery string, cause error) error {
	return &Problem{Code: code, Message: message, Recovery: recovery, cause: cause}
}

// StorageProblem is the one "profile storage is unavailable" problem.
func StorageProblem(cause error) error {
	return WrapProblem("storage_error", "Profile storage is unavailable.",
		"Review the local profile storage diagnostics and retry.", cause)
}

// ProblemCode returns err's bounded code; an error that is not a Problem reads as a
// storage error.
func ProblemCode(err error) string {
	var p *Problem
	if errors.As(err, &p) {
		return p.Code
	}
	return "storage_error"
}

type authoredProfile struct {
	FormatVersion    int                  `yaml:"format-version"`
	Kind             string               `yaml:"kind"`
	ID               string               `yaml:"id"`
	Version          string               `yaml:"version"`
	Name             string               `yaml:"name"`
	Description      string               `yaml:"description"`
	Role             string               `yaml:"role"`
	Type             string               `yaml:"type,omitempty"`
	Priority         *int                 `yaml:"priority,omitempty"`
	MayTag           []string             `yaml:"may-tag,omitempty"`
	AwaitAnnotations string               `yaml:"await-annotations,omitempty"`
	Stages           map[string]string    `yaml:"stages,omitempty"`
	ReplyShape       string               `yaml:"reply-shape,omitempty"`
	Execution        string               `yaml:"execution"`
	Trigger          authoredTrigger      `yaml:"trigger"`
	Context          []authoredContext    `yaml:"context"`
	Output           authoredOutput       `yaml:"output"`
	Authority        []string             `yaml:"authority-requests"`
	AllowedProfiles  []string             `yaml:"allowed-profiles,omitempty"`
	Requirements     authoredRequirements `yaml:"requirements"`
	Limits           authoredLimits       `yaml:"limits"`
	Failure          authoredFailure      `yaml:"failure"`
	Tags             []string             `yaml:"tags,omitempty"`
	Presentation     authoredPresentation `yaml:"presentation,omitempty"`
	License          string               `yaml:"license,omitempty"`
	Provenance       string               `yaml:"provenance,omitempty"`
}

type authoredTrigger struct {
	Event        string   `yaml:"event"`
	States       []string `yaml:"states,omitempty"`
	IgnoreOrigin string   `yaml:"ignore-origin,omitempty"`
	Debounce     string   `yaml:"debounce,omitempty"`
}

type authoredContext struct {
	Kind     string `yaml:"kind"`
	Selector string `yaml:"selector,omitempty"`
	Required *bool  `yaml:"required,omitempty"`
	MaxBytes *int   `yaml:"max-bytes,omitempty"`
}

type authoredOutput struct {
	Kind   string `yaml:"kind"`
	Schema string `yaml:"schema,omitempty"`
}

type authoredRequirements struct {
	Capabilities []string            `yaml:"capabilities"`
	Skills       []string            `yaml:"skills,omitempty"`
	Destination  authoredDestination `yaml:"destination,omitempty"`
}

type authoredDestination struct {
	Locality string `yaml:"locality,omitempty"`
}

type authoredLimits struct {
	Timeout        string `yaml:"timeout"`
	MaxHops        int    `yaml:"max-hops"`
	MaxDepth       int    `yaml:"max-depth"`
	MaxInputBytes  *int   `yaml:"max-input-bytes,omitempty"`
	MaxOutputBytes *int   `yaml:"max-output-bytes,omitempty"`
	MaxTokens      *int   `yaml:"max-tokens,omitempty"`
	MaxRetries     *int   `yaml:"max-retries,omitempty"`
	MaxConcurrency *int   `yaml:"max-concurrency,omitempty"`
	LoopBudget     *int64 `yaml:"loop-budget,omitempty"`
	MaxGroupTokens *int64 `yaml:"max-group-tokens,omitempty"`
	MaxAgentTokens *int64 `yaml:"max-agent-tokens,omitempty"`
}

type authoredFailure struct {
	MissingRequiredContext string `yaml:"missing-required-context"`
	UnavailableCapability  string `yaml:"unavailable-capability"`
	Timeout                string `yaml:"timeout"`
	MalformedOutput        string `yaml:"malformed-output"`
}

type authoredPresentation struct {
	Job string `yaml:"job,omitempty"`
}

// CompiledProfile is the normalized, inert value stored beside exact source bytes.
// Fixed structs (rather than maps) make its canonical JSON field order explicit.
type CompiledProfile struct {
	FormatVersion    int                  `json:"format_version"`
	Kind             string               `json:"kind"`
	ID               string               `json:"id"`
	Version          string               `json:"version"`
	Name             string               `json:"name"`
	Description      string               `json:"description"`
	Role             string               `json:"role"`
	Type             string               `json:"type,omitempty"`
	Priority         int                  `json:"priority,omitempty"`
	MayTag           []string             `json:"may_tag,omitempty"`
	AwaitAnnotations string               `json:"await_annotations,omitempty"`
	Stages           map[string]string    `json:"stages,omitempty"`
	ReplyShape       string               `json:"reply_shape,omitempty"`
	Execution        string               `json:"execution"`
	Trigger          CompiledTrigger      `json:"trigger"`
	Context          []CompiledContext    `json:"context"`
	Output           CompiledOutput       `json:"output"`
	Authority        []string             `json:"authority_requests"`
	AllowedProfiles  []string             `json:"allowed_profiles"`
	Requirements     CompiledRequirements `json:"requirements"`
	Limits           CompiledLimits       `json:"limits"`
	Failure          CompiledFailure      `json:"failure"`
	Tags             []string             `json:"tags"`
	Presentation     CompiledPresentation `json:"presentation"`
	License          string               `json:"license,omitempty"`
	Provenance       string               `json:"provenance,omitempty"`
	Instructions     string               `json:"instructions"`
}

type CompiledTrigger struct {
	Event        string   `json:"event"`
	States       []string `json:"states"`
	IgnoreOrigin string   `json:"ignore_origin"`
	Debounce     string   `json:"debounce"`
}

// CompiledContext is one compiled context selection.
type CompiledContext = ContextSelection

type CompiledOutput struct {
	Kind   string `json:"kind"`
	Schema string `json:"schema"`
}

type CompiledRequirements struct {
	Capabilities []string            `json:"capabilities"`
	Skills       []string            `json:"skills"`
	Destination  CompiledDestination `json:"destination"`
}

type CompiledDestination struct {
	Locality string `json:"locality"`
}

type CompiledLimits struct {
	Timeout        string `json:"timeout"`
	MaxHops        int    `json:"max_hops"`
	MaxDepth       int    `json:"max_depth"`
	MaxInputBytes  int    `json:"max_input_bytes"`
	MaxOutputBytes int    `json:"max_output_bytes"`
	MaxTokens      int    `json:"max_tokens"`
	MaxRetries     int    `json:"max_retries"`
	MaxConcurrency int    `json:"max_concurrency"`
	LoopBudget     int64  `json:"loop_budget,omitempty"`
	MaxGroupTokens int64  `json:"max_group_tokens,omitempty"`
	MaxAgentTokens int64  `json:"max_agent_tokens,omitempty"`
}

type CompiledFailure struct {
	MissingRequiredContext string `json:"missing_required_context"`
	UnavailableCapability  string `json:"unavailable_capability"`
	Timeout                string `json:"timeout"`
	MalformedOutput        string `json:"malformed_output"`
}

type CompiledPresentation struct {
	Job string `json:"job,omitempty"`
}

type Document struct {
	Source       []byte
	SourceDigest string
	BundleDigest string
	Canonical    []byte
	Profile      CompiledProfile
}

type Preview struct {
	ProfileID          string          `json:"profile_id"`
	SourceDigest       string          `json:"source_digest"`
	BundleDigest       string          `json:"bundle_digest"`
	StateToken         string          `json:"state_token"`
	SelectionState     string          `json:"selection_state"`
	RuntimeEffects     bool            `json:"runtime_effects"`
	When               string          `json:"when"`
	Sees               []string        `json:"sees"`
	Does               string          `json:"does"`
	Appears            string          `json:"appears"`
	Destination        string          `json:"destination"`
	Limits             []string        `json:"limits"`
	RequestedAuthority []string        `json:"requested_authority"`
	AuthorityGranted   bool            `json:"authority_granted"`
	Normalized         CompiledProfile `json:"normalized"`
}

func compileProfile(source authoredProfile, instructions string) (CompiledProfile, error) {
	if source.FormatVersion != FormatVersion {
		return CompiledProfile{}, problem("incompatible_profile", "format-version",
			"This profile format version is not supported.", "Use format-version 1.")
	}
	if source.Kind != ProfileKind {
		return CompiledProfile{}, Invalid("kind", "Use the orchestration profile kind.")
	}
	if !ProfileIDPattern.MatchString(source.ID) {
		return CompiledProfile{}, Invalid("id", "Use a lowercase profile ID up to 64 characters.")
	}
	if !ValidSemver(source.Version) {
		return CompiledProfile{}, Invalid("version", "Use a bounded SemVer 2.0 version.")
	}
	if err := boundedText("name", source.Name, 1, 100); err != nil {
		return CompiledProfile{}, err
	}
	if err := boundedText("description", source.Description, 1, 500); err != nil {
		return CompiledProfile{}, err
	}
	if err := boundedText("license", source.License, 0, 200); err != nil {
		return CompiledProfile{}, err
	}
	if err := boundedText("provenance", source.Provenance, 0, 1000); err != nil {
		return CompiledProfile{}, err
	}
	// `role` is an ACCEPTED AUTHORING ALIAS only: the legacy names remain valid
	// input so already-selected v1 documents keep their exact canonical bytes
	// and digests (content-addressing discipline), but AgentType() is the one
	// authoritative taxonomy concept downstream — no runtime surface keys on
	// these alias strings.
	if !oneOf(source.Role, "reviewer", "follower", "coordinator", "course-corrector", "delegate") {
		return CompiledProfile{}, Invalid("role", "Choose a supported profile role.")
	}
	if !oneOf(source.Execution, "stateless-review", "managed-turn") {
		return CompiledProfile{}, Invalid("execution", "Choose a supported execution primitive.")
	}
	if (source.Role == "reviewer" && source.Execution != "stateless-review") ||
		((source.Role == "follower" || source.Role == "coordinator" || source.Role == "delegate") && source.Execution != "managed-turn") {
		return CompiledProfile{}, Invalid("execution", "The role and execution primitive are incompatible.")
	}

	trigger, err := compileTrigger(source.Trigger)
	if err != nil {
		return CompiledProfile{}, err
	}
	contexts, err := compileContexts(source.Context)
	if err != nil {
		return CompiledProfile{}, err
	}
	output, err := compileOutput(source.Output)
	if err != nil {
		return CompiledProfile{}, err
	}
	capabilities, err := compileSet("requirements.capabilities", source.Requirements.Capabilities, 16, true)
	if err != nil {
		return CompiledProfile{}, err
	}
	for _, capability := range capabilities {
		if !oneOf(capability, "one-shot-inference", "managed-turn", "project-read", "governance-read",
			"approval-response", "profile-launch", "task-control") {
			return CompiledProfile{}, Invalid("requirements.capabilities", "Use only supported capability names.")
		}
	}
	if source.Execution == "stateless-review" && !contains(capabilities, "one-shot-inference") {
		return CompiledProfile{}, Invalid("requirements.capabilities", "Stateless review requires one-shot inference.")
	}
	if source.Execution == "managed-turn" && !contains(capabilities, "managed-turn") {
		return CompiledProfile{}, Invalid("requirements.capabilities", "Managed execution requires managed-turn capability.")
	}
	skills, err := compileOpenIDSet("requirements.skills", source.Requirements.Skills, 16)
	if err != nil {
		return CompiledProfile{}, err
	}
	locality := source.Requirements.Destination.Locality
	if locality == "" {
		locality = "local-only"
	}
	if !oneOf(locality, "local-only", "no-network-destination", "explicit-local-or-remote") {
		return CompiledProfile{}, Invalid("requirements.destination.locality", "Choose a supported destination locality.")
	}
	authority, err := compileSet("authority-requests", source.Authority, 8, false)
	if err != nil {
		return CompiledProfile{}, err
	}
	for _, request := range authority {
		if !oneOf(request, "advise", "respond-approval", "draft-reply", "reply", "launch-profile",
			"request-interrupt", "resume-correction", "send-message") {
			return CompiledProfile{}, Invalid("authority-requests", "Use only supported authority request names.")
		}
	}
	allowedProfiles, err := compileProfileIDSet("allowed-profiles", source.AllowedProfiles, 16)
	if err != nil {
		return CompiledProfile{}, err
	}
	if contains(allowedProfiles, source.ID) {
		return CompiledProfile{}, Invalid("allowed-profiles", "A profile cannot allowlist itself.")
	}
	tags, err := compileTags(source.Tags)
	if err != nil {
		return CompiledProfile{}, err
	}
	limits, err := compileLimits(source.Limits)
	if err != nil {
		return CompiledProfile{}, err
	}
	failure, err := compileFailure(source.Failure)
	if err != nil {
		return CompiledProfile{}, err
	}
	agentType, err := compileAgentType(source.Type, source.Execution)
	if err != nil {
		return CompiledProfile{}, err
	}
	if err := validateCombinations(source.Role, source.Execution, agentType, trigger, contexts, output, authority,
		allowedProfiles, capabilities, source.Presentation.Job); err != nil {
		return CompiledProfile{}, err
	}
	// Passive/acting is type-level (plan §2): a declared follower observes and
	// tags but never requests acting authority — those requests make it a helper.
	if agentType == "follower" || agentType == "reviewer" {
		for _, request := range authority {
			if oneOf(request, "reply", "launch-profile", "request-interrupt", "resume-correction", "send-message") {
				return CompiledProfile{}, Invalid("type",
					"Passive agent types cannot request acting authority; declare a helper instead.")
			}
		}
	}
	priority, err := compilePriority(source.Priority)
	if err != nil {
		return CompiledProfile{}, err
	}
	if source.AwaitAnnotations != "" && source.AwaitAnnotations != "always" && source.AwaitAnnotations != "never" {
		return CompiledProfile{}, &Problem{Field: "await-annotations", Message: "await-annotations must be always or never"}
	}
	mayTag, err := compileMayTag(source.MayTag)
	if err != nil {
		return CompiledProfile{}, err
	}
	stages, err := compileStages(source.Stages)
	if err != nil {
		return CompiledProfile{}, err
	}
	if err := boundedText("reply-shape", source.ReplyShape, 0, maxReplyShapeRunes); err != nil {
		return CompiledProfile{}, err
	}

	return CompiledProfile{
		FormatVersion: source.FormatVersion, Kind: source.Kind, ID: source.ID, Version: source.Version,
		Name: source.Name, Description: source.Description, Role: source.Role, Execution: source.Execution,
		Type: agentType, Priority: priority, MayTag: mayTag, Stages: stages, ReplyShape: source.ReplyShape,
		AwaitAnnotations: source.AwaitAnnotations,
		Trigger:          trigger, Context: contexts, Output: output, Authority: authority,
		AllowedProfiles: allowedProfiles,
		Requirements: CompiledRequirements{Capabilities: capabilities, Skills: skills,
			Destination: CompiledDestination{Locality: locality}},
		Limits: limits, Failure: failure, Tags: tags,
		Presentation: CompiledPresentation{Job: source.Presentation.Job}, License: source.License,
		Provenance: source.Provenance, Instructions: normalizeLineEndings(instructions),
	}, nil
}

func compileTrigger(source authoredTrigger) (CompiledTrigger, error) {
	// The trigger vocabulary is the published signal catalog plus the review
	// lane's pretool.action — never a private enum that can drift from what
	// the pumps actually emit.
	if source.Event != "pretool.action" && source.Event != "*" && !KnownSignal(source.Event) {
		return CompiledTrigger{}, Invalid("trigger.event", "Choose a published signal kind or pretool.action.")
	}
	states, err := compileOpenIDSet("trigger.states", source.States, 16)
	if err != nil {
		return CompiledTrigger{}, err
	}
	ignoreOrigin := source.IgnoreOrigin
	if ignoreOrigin == "" {
		ignoreOrigin = "self"
	}
	if !oneOf(ignoreOrigin, "self", "none") {
		return CompiledTrigger{}, Invalid("trigger.ignore-origin", "Choose self or none.")
	}
	debounce := source.Debounce
	if debounce == "" {
		debounce = "0s"
	}
	if _, err := boundedDuration(debounce, 0, 10*time.Minute); err != nil {
		return CompiledTrigger{}, Invalid("trigger.debounce", "Use a simple duration from 0s through 10m.")
	}
	return CompiledTrigger{Event: source.Event, States: states, IgnoreOrigin: ignoreOrigin, Debounce: debounce}, nil
}

func compileContexts(source []authoredContext) ([]CompiledContext, error) {
	if len(source) < 1 || len(source) > 16 {
		return nil, Invalid("context", "Provide between 1 and 16 bounded context requests.")
	}
	out := make([]CompiledContext, 0, len(source))
	for index, item := range source {
		field := fmt.Sprintf("context[%d]", index)
		if !oneOf(item.Kind, "pretool-action", "permission-scope", "task.final-response", "task.messages", "session.messages",
			"project-files", "task.changes", "task.checks", "task.plan", "governance-decisions", "prior-claims", "session.tags") {
			return nil, Invalid(field+".kind", "Choose a supported context kind.")
		}
		if item.Selector != "" && !stableIDPattern.MatchString(item.Selector) {
			return nil, Invalid(field+".selector", "Use a bounded stable selector ID.")
		}
		required := false
		if item.Required != nil {
			required = *item.Required
		}
		maxBytes := 65_536
		if item.MaxBytes != nil {
			maxBytes = *item.MaxBytes
		}
		if maxBytes < 1 || maxBytes > maxContextBytes {
			return nil, Invalid(field+".max-bytes", "Choose a context bound from 1 through 262144 bytes.")
		}
		out = append(out, CompiledContext{Kind: item.Kind, Selector: item.Selector,
			Required: required, MaxBytes: maxBytes})
	}
	return out, nil
}

func compileOutput(source authoredOutput) (CompiledOutput, error) {
	if !oneOf(source.Kind, "review-recommendation", "approval-response", "advice", "draft-reply",
		"stage-classification", "intervention") {
		return CompiledOutput{}, Invalid("output.kind", "Choose a supported output kind.")
	}
	schema := "builtin/" + source.Kind + "-v1"
	if source.Schema != "" && source.Schema != schema {
		return CompiledOutput{}, Invalid("output.schema", "Use the matching built-in output schema.")
	}
	return CompiledOutput{Kind: source.Kind, Schema: schema}, nil
}

func compileLimits(source authoredLimits) (CompiledLimits, error) {
	if _, err := boundedDuration(source.Timeout, time.Second, 10*time.Minute); err != nil {
		return CompiledLimits{}, Invalid("limits.timeout", "Use a simple duration from 1s through 10m.")
	}
	if source.MaxHops < 1 || source.MaxHops > 4 {
		return CompiledLimits{}, Invalid("limits.max-hops", "Choose max-hops from 1 through 4.")
	}
	if source.MaxDepth < 1 || source.MaxDepth > 4 {
		return CompiledLimits{}, Invalid("limits.max-depth", "Choose max-depth from 1 through 4.")
	}
	input, err := optionalInt("limits.max-input-bytes", source.MaxInputBytes, 262_144, 1, 1_048_576)
	if err != nil {
		return CompiledLimits{}, err
	}
	output, err := optionalInt("limits.max-output-bytes", source.MaxOutputBytes, 32_768, 1, 262_144)
	if err != nil {
		return CompiledLimits{}, err
	}
	tokens, err := optionalInt("limits.max-tokens", source.MaxTokens, 8_192, 1, 65_536)
	if err != nil {
		return CompiledLimits{}, err
	}
	retries, err := optionalInt("limits.max-retries", source.MaxRetries, 0, 0, 3)
	if err != nil {
		return CompiledLimits{}, err
	}
	concurrency, err := optionalInt("limits.max-concurrency", source.MaxConcurrency, 1, 1, 4)
	if err != nil {
		return CompiledLimits{}, err
	}
	// The v2 budget keys are OWNER budgets (plan §2): zero means "use the host's
	// shipped default configuration"; the profile never invents one.
	loopBudget, err := optionalInt64("limits.loop-budget", source.LoopBudget)
	if err != nil {
		return CompiledLimits{}, err
	}
	groupTokens, err := optionalInt64("limits.max-group-tokens", source.MaxGroupTokens)
	if err != nil {
		return CompiledLimits{}, err
	}
	agentTokens, err := optionalInt64("limits.max-agent-tokens", source.MaxAgentTokens)
	if err != nil {
		return CompiledLimits{}, err
	}
	return CompiledLimits{Timeout: source.Timeout, MaxHops: source.MaxHops, MaxDepth: source.MaxDepth,
		MaxInputBytes: input, MaxOutputBytes: output, MaxTokens: tokens, MaxRetries: retries,
		MaxConcurrency: concurrency, LoopBudget: loopBudget, MaxGroupTokens: groupTokens,
		MaxAgentTokens: agentTokens}, nil
}

func compileFailure(source authoredFailure) (CompiledFailure, error) {
	values := []struct {
		field string
		value string
	}{
		{"failure.missing-required-context", source.MissingRequiredContext},
		{"failure.unavailable-capability", source.UnavailableCapability},
		{"failure.timeout", source.Timeout},
		{"failure.malformed-output", source.MalformedOutput},
	}
	for _, item := range values {
		if !oneOf(item.value, "block", "skip", "record-unavailable") {
			return CompiledFailure{}, Invalid(item.field, "Choose block, skip, or record-unavailable.")
		}
	}
	return CompiledFailure(source), nil
}

func validateCombinations(role, execution, agentType string, trigger CompiledTrigger, contexts []CompiledContext,
	output CompiledOutput, authority, allowedProfiles, capabilities []string, job string) error {
	valid := false
	switch output.Kind {
	case "review-recommendation":
		valid = role == "reviewer" && (len(authority) == 0 || equalSet(authority, "advise"))
	case "approval-response":
		valid = role == "reviewer" && equalSet(authority, "respond-approval") &&
			trigger.Event == "approval.pending" && contains(capabilities, "approval-response")
	case "advice":
		valid = len(authority) == 0 || equalSet(authority, "advise")
	case "draft-reply":
		valid = role == "follower" && (equalSet(authority, "draft-reply") || equalSet(authority, "reply"))
	case "stage-classification":
		valid = (role == "coordinator" || role == "delegate") && equalSet(authority, "launch-profile") &&
			len(allowedProfiles) > 0 && contains(capabilities, "profile-launch")
	case "intervention":
		valid = (agentType == "helper" || (agentType == "" && role == "course-corrector")) && len(authority) > 0
		for _, item := range authority {
			valid = valid && oneOf(item, "advise", "request-interrupt", "resume-correction", "send-message")
		}
		if contains(authority, "resume-correction") && !contains(authority, "request-interrupt") {
			valid = false
		}
		if (contains(authority, "request-interrupt") || contains(authority, "resume-correction")) &&
			!contains(capabilities, "task-control") {
			valid = false
		}
	}
	if !valid {
		return Invalid("authority-requests", "The role, output, capabilities, and requested authority are incompatible.")
	}
	if output.Kind != "stage-classification" && len(allowedProfiles) != 0 {
		return Invalid("allowed-profiles", "Only stage classification may allowlist child profiles.")
	}
	for _, context := range contexts {
		if context.Kind == "project-files" && !contains(capabilities, "project-read") {
			return Invalid("requirements.capabilities", "Project-file context requires project-read capability.")
		}
		if context.Kind == "governance-decisions" && !contains(capabilities, "governance-read") {
			return Invalid("requirements.capabilities", "Governance context requires governance-read capability.")
		}
	}
	if job != "" {
		jobValid := (job == "review-tool-calls" && role == "reviewer") ||
			(job == "answer-from-project-docs" && role == "follower") ||
			(job == "keep-work-aligned" && role == "course-corrector") ||
			(job == "coordinate-project-plan" && (role == "coordinator" || role == "delegate"))
		if !jobValid {
			return Invalid("presentation.job", "The presentation job does not match the profile role.")
		}
	}
	if execution == "stateless-review" && role != "reviewer" && role != "course-corrector" {
		return Invalid("execution", "This role cannot use stateless review.")
	}
	return nil
}

// Invalid is the one "this profile field is invalid" problem, naming the schema path.
func Invalid(field, recovery string) error {
	return problem("invalid_profile", field, "This profile field is invalid.", recovery)
}

func boundedText(field, value string, minRunes, maxRunes int) error {
	count := utf8.RuneCountInString(value)
	if count < minRunes || count > maxRunes || (minRunes > 0 && strings.TrimSpace(value) == "") {
		return Invalid(field, "Use bounded, nonempty text for this field.")
	}
	return nil
}

// ValidSemver reports whether value is a semantic version the profile format accepts.
func ValidSemver(value string) bool {
	if value == "" || len(value) > 64 || !ascii(value) {
		return false
	}
	coreAndPre, build, hasBuild := strings.Cut(value, "+")
	if hasBuild && !validIdentifiers(build, false) {
		return false
	}
	if strings.Contains(build, "+") {
		return false
	}
	core, pre, hasPre := strings.Cut(coreAndPre, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !numericIdentifier(part) {
			return false
		}
	}
	return !hasPre || validIdentifiers(pre, true)
}

func validIdentifiers(value string, rejectNumericLeadingZero bool) bool {
	if value == "" {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if part == "" {
			return false
		}
		numeric := true
		for _, char := range part {
			if (char < '0' || char > '9') && (char < 'A' || char > 'Z') &&
				(char < 'a' || char > 'z') && char != '-' {
				return false
			}
			if char < '0' || char > '9' {
				numeric = false
			}
		}
		if rejectNumericLeadingZero && numeric && len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

func numericIdentifier(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func ascii(value string) bool {
	for index := range len(value) {
		if value[index] > 0x7f {
			return false
		}
	}
	return true
}

func compileSet(field string, values []string, max int, requireNonempty bool) ([]string, error) {
	if len(values) > max || (requireNonempty && len(values) == 0) {
		return nil, Invalid(field, "Use a bounded nonempty set for this field.")
	}
	out := append([]string(nil), values...)
	for _, value := range out {
		if !stableIDPattern.MatchString(value) {
			return nil, Invalid(field, "Use bounded stable IDs in this field.")
		}
	}
	sort.Strings(out)
	for index := 1; index < len(out); index++ {
		if out[index] == out[index-1] {
			return nil, Invalid(field, "Duplicate list values are not allowed.")
		}
	}
	return out, nil
}

func compileOpenIDSet(field string, values []string, max int) ([]string, error) {
	return compileSet(field, values, max, false)
}

func compileProfileIDSet(field string, values []string, max int) ([]string, error) {
	if len(values) > max {
		return nil, Invalid(field, "Use no more than 16 profile IDs.")
	}
	out := append([]string(nil), values...)
	for _, value := range out {
		if !ProfileIDPattern.MatchString(value) {
			return nil, Invalid(field, "Use valid lowercase profile IDs.")
		}
	}
	sort.Strings(out)
	for index := 1; index < len(out); index++ {
		if out[index] == out[index-1] {
			return nil, Invalid(field, "Duplicate profile IDs are not allowed.")
		}
	}
	return out, nil
}

// Format v2 (agents redesign, plan §9) additive bounds. All v2 frontmatter is
// optional so every v1 profile keeps its exact canonical bytes and digests.
const (
	maxMayTagEntries   = 16
	maxMayTagBytes     = 64 // matches the store's orchestration_tag CHECK
	maxStageSelectors  = 16
	maxStagePromptSize = maxYAMLScalar
	maxReplyShapeRunes = 2000
	minAgentPriority   = -1000 // matches the binding priority CHECK
	maxAgentPriority   = 1000
	maxTokenBudget     = 1_000_000_000
)

// compileAgentType resolves the redesign taxonomy type. An authored `type` is
// authoritative; otherwise the recorded deterministic legacy mapping applies
// (plan §8): coordinator and course-corrector become helper, a follower that
// requested `reply` authority becomes helper, a draft-only follower stays
// follower, and reviewer stays reviewer. The raw legacy role is kept for
// display in CompiledProfile.Role.
func compileAgentType(authored, execution string) (string, error) {
	if authored == "" {
		return "", nil
	}
	if !oneOf(authored, "reviewer", "follower", "helper") {
		return "", Invalid("type", "Choose reviewer, follower, or helper.")
	}
	if (authored == "reviewer") != (execution == "stateless-review") {
		return "", Invalid("type", "The agent type and execution primitive are incompatible.")
	}
	return authored, nil
}

// AgentType returns the effective redesign taxonomy type: the authored `type`
// when present, else the deterministic legacy-role mapping (plan §8).
func (p CompiledProfile) AgentType() string {
	if p.Type != "" {
		return p.Type
	}
	switch p.Role {
	case "reviewer":
		return "reviewer"
	case "follower":
		if contains(p.Authority, "reply") {
			return "helper"
		}
		return "follower"
	case "coordinator", "course-corrector", "delegate":
		return "helper"
	}
	return p.Role
}

func compilePriority(value *int) (int, error) {
	if value == nil {
		return 0, nil
	}
	if *value < minAgentPriority || *value > maxAgentPriority {
		return 0, Invalid("priority", "Choose a priority from -1000 through 1000.")
	}
	return *value, nil
}

func compileMayTag(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > maxMayTagEntries {
		return nil, Invalid("may-tag", "Declare no more than 16 tags.")
	}
	out := append([]string(nil), values...)
	for _, value := range out {
		// A declared tag becomes half of a governance fact key
		// (agent:<binding>:<tag>), so it uses the same stable-id charset as
		// every other identifier — no whitespace, colons, or control bytes.
		if len(value) > maxMayTagBytes || !stableIDPattern.MatchString(value) {
			return nil, Invalid("may-tag", "Use nonempty stable-id declared tags up to 64 bytes.")
		}
	}
	sort.Strings(out)
	for index := 1; index < len(out); index++ {
		if out[index] == out[index-1] {
			return nil, Invalid("may-tag", "Duplicate declared tags are not allowed.")
		}
	}
	return out, nil
}

// compileStages validates the OPEN selector→prompt map. Selectors are checked
// against the published signal catalog — the one vocabulary the lower layers
// emit — and deliberately against nothing else: there is no fixed key set and
// no stage enum anywhere in code (owner rule, plan §2).
func compileStages(stages map[string]string) (map[string]string, error) {
	if len(stages) == 0 {
		return nil, nil
	}
	if len(stages) > maxStageSelectors {
		return nil, Invalid("stages", "Map no more than 16 signal selectors.")
	}
	out := make(map[string]string, len(stages))
	for selector, prompt := range stages {
		if selector != "*" && !KnownSignal(selector) {
			return nil, Invalid("stages", "Every stages key must be a published signal selector.")
		}
		if strings.TrimSpace(prompt) == "" || len(prompt) > maxStagePromptSize {
			return nil, Invalid("stages", "Use a nonempty bounded prompt for every signal selector.")
		}
		out[selector] = normalizeLineEndings(prompt)
	}
	return out, nil
}

func compileTags(values []string) ([]string, error) {
	if len(values) > 16 {
		return nil, Invalid("tags", "Use no more than 16 tags.")
	}
	out := append([]string(nil), values...)
	for _, value := range out {
		if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > 32 {
			return nil, Invalid("tags", "Use nonempty tags up to 32 characters.")
		}
	}
	sort.Strings(out)
	for index := 1; index < len(out); index++ {
		if out[index] == out[index-1] {
			return nil, Invalid("tags", "Duplicate tags are not allowed.")
		}
	}
	return out, nil
}

func boundedDuration(value string, min, max time.Duration) (time.Duration, error) {
	match := durationPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, errors.New("invalid duration")
	}
	number, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, err
	}
	multiplier := time.Second
	switch match[2] {
	case "ms":
		multiplier = time.Millisecond
	case "m":
		multiplier = time.Minute
	}
	if number > int64(max/multiplier) {
		return 0, errors.New("duration too large")
	}
	duration := time.Duration(number) * multiplier
	if duration < min || duration > max {
		return 0, errors.New("duration outside bounds")
	}
	return duration, nil
}

// Duration reads a compiled duration (`limits.timeout`, `trigger.debounce`) with
// the one grammar the compiler validated it against, so hosts never carry a
// second parser for profile durations.
func Duration(value string) (time.Duration, error) {
	return boundedDuration(value, 0, 10*time.Minute)
}

func optionalInt(field string, value *int, fallback, min, max int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	if *value < min || *value > max {
		return 0, Invalid(field, "Choose a value within the documented limit.")
	}
	return *value, nil
}

func optionalInt64(field string, value *int64) (int64, error) {
	if value == nil {
		return 0, nil
	}
	if *value < 0 || *value > maxTokenBudget {
		return 0, Invalid(field, "Choose a non-negative bounded budget, or 0 for the shipped default.")
	}
	return *value, nil
}

func normalizeLineEndings(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

func oneOf(value string, allowed ...string) bool { return contains(allowed, value) }

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func equalSet(values []string, expected ...string) bool {
	if len(values) != len(expected) {
		return false
	}
	want := append([]string(nil), expected...)
	sort.Strings(want)
	for index := range values {
		if values[index] != want[index] {
			return false
		}
	}
	return true
}
