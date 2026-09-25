package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"crossing-guard/infer"
	"crossing-guard/internal/observation"
	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

const maxReviewHostWorkers = 16

var reviewHost *orchestrationReviewHost

type reviewRuntimeBinding struct {
	record  store.ReviewBinding
	profile orchestration.Profile
	path    infer.RequestPath
}

type reviewBindingCommand struct {
	ProfileID             string
	ProfileSourceDigest   string
	ProfileBundleDigest   string
	Endpoint              string
	Model                 string
	TimeoutMS             int
	Effect                string
	ApprovalSubdeadlineMS int
	// AnswerChoicePrompts is the operator's grant for this reviewer to answer a
	// held call's questions, not just permit or refuse it.
	AnswerChoicePrompts bool
	RuntimeFilter       string
	ExpectedStateToken  string
}

type orchestrationReviewHost struct {
	mu                         sync.RWMutex
	ix                         *store.Index
	profiles                   *profilefs.Owner
	runtimeBinding             *reviewRuntimeBinding
	startupProblem             string
	ctx                        context.Context
	cancel                     context.CancelFunc
	workers                    chan struct{}
	wg                         sync.WaitGroup
	closing                    bool
	unsubscribeApprovalPending func()
}

func newOrchestrationReviewHost(ix *store.Index, profiles *profilefs.Owner) (*orchestrationReviewHost, error) {
	if ix == nil || profiles == nil {
		return nil, errors.New("review store and profile owner are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	host := &orchestrationReviewHost{ix: ix, profiles: profiles, ctx: ctx, cancel: cancel,
		workers: make(chan struct{}, maxReviewHostWorkers)}
	if _, err := ix.RecoverReviewInvocationsUnknown(time.Now().Unix()); err != nil {
		cancel()
		return nil, fmt.Errorf("recover report reviews: %w", err)
	}
	binding, found, err := ix.ReviewBinding()
	if err != nil {
		cancel()
		return nil, err
	}
	if found && binding.State == "enabled" {
		runtimeBinding, resolveErr := host.resolveBinding(binding)
		if resolveErr != nil {
			host.startupProblem = "The enabled review binding could not resolve its pinned profile or request path."
		} else {
			host.runtimeBinding = runtimeBinding
		}
	}
	host.unsubscribeApprovalPending = approvals.subscribePending(host.offerApproval)
	return host, nil
}

func (host *orchestrationReviewHost) close() {
	if host == nil {
		return
	}
	host.mu.Lock()
	if host.closing {
		host.mu.Unlock()
		return
	}
	host.closing = true
	host.runtimeBinding = nil
	if host.unsubscribeApprovalPending != nil {
		host.unsubscribeApprovalPending()
		host.unsubscribeApprovalPending = nil
	}
	host.cancel()
	host.mu.Unlock()
	host.wg.Wait()
	_, _ = host.ix.RecoverReviewInvocationsUnknown(time.Now().Unix())
}

func (host *orchestrationReviewHost) resolveBinding(binding store.ReviewBinding) (*reviewRuntimeBinding, error) {
	detail, err := host.profiles.GetRevision(binding.ProfileID, binding.ProfileSourceDigest, binding.ProfileBundleDigest)
	if err != nil || detail.Normalized == nil {
		return nil, errors.New("pinned profile revision unavailable")
	}
	profile, err := sliceCProfile(*detail.Normalized, binding.ProfileSourceDigest, binding.ProfileBundleDigest)
	if err != nil {
		return nil, err
	}
	if binding.InstructionDigest != orchestration.InstructionDigest(profile.Instructions) ||
		binding.TimeoutMS < 1 || time.Duration(binding.TimeoutMS)*time.Millisecond > profile.Timeout ||
		binding.MaxInputBytes != profile.MaxInputBytes || binding.MaxOutputBytes != profile.MaxOutputBytes ||
		binding.MaxTokens != profile.MaxTokens || binding.MaxConcurrency != profile.MaxConcurrency {
		return nil, errors.New("binding no longer matches pinned profile limits")
	}
	if binding.Effect == "" {
		binding.Effect = "report-only"
	}
	if binding.Effect != "report-only" && binding.Effect != "delegated-first" {
		return nil, errors.New("binding effect is unsupported")
	}
	if binding.Effect == "delegated-first" && (binding.ApprovalSubdeadlineMS < 250 || binding.ApprovalSubdeadlineMS > binding.TimeoutMS) {
		return nil, errors.New("binding approval subdeadline is invalid")
	}
	profile.DelegatedApproval = binding.Effect == "delegated-first"
	profile.Timeout = time.Duration(binding.TimeoutMS) * time.Millisecond
	path, err := infer.NewLocalOllamaPath(binding.Endpoint, binding.Model,
		binding.MaxInputBytes, binding.MaxOutputBytes, binding.MaxTokens)
	if err != nil || path.Kind != binding.RequestPathKind || path.Digest != binding.RequestPathDigest {
		return nil, errors.New("binding request path identity mismatch")
	}
	return &reviewRuntimeBinding{record: binding, profile: profile, path: path}, nil
}

func sliceCProfile(compiled profilefs.CompiledProfile, sourceDigest, bundleDigest string) (orchestration.Profile, error) {
	reportShape := compiled.Trigger.Event == "pretool.action" && compiled.Output.Kind == "review-recommendation" &&
		compiled.Output.Schema == orchestration.ReviewSchemaID && len(compiled.Context) == 1 &&
		compiled.Context[0].Kind == "pretool-action" && onlyContainsStrings(compiled.Authority, "advise")
	delegatedShape := compiled.Trigger.Event == "approval.pending" && compiled.Output.Kind == "approval-response" &&
		compiled.Output.Schema == "builtin/approval-response-v1" && len(compiled.Context) == 1 &&
		compiled.Context[0].Kind == "permission-scope" && containsString(compiled.Authority, "respond-approval") &&
		containsString(compiled.Requirements.Capabilities, "approval-response")
	if compiled.AgentType() != "reviewer" || compiled.Execution != "stateless-review" || len(compiled.Trigger.States) != 0 ||
		(!reportShape && !delegatedShape) || !compiled.Context[0].Required ||
		len(compiled.AllowedProfiles) != 0 || compiled.Limits.MaxRetries != 0 ||
		!oneOfString(compiled.Requirements.Destination.Locality, "local-only", "no-network-destination") ||
		!containsString(compiled.Requirements.Capabilities, "one-shot-inference") {
		return orchestration.Profile{}, errors.New("profile is incompatible with report-only one-shot review")
	}
	timeout, err := time.ParseDuration(compiled.Limits.Timeout)
	if err != nil || timeout < time.Second || timeout > 120*time.Second {
		return orchestration.Profile{}, errors.New("profile timeout is incompatible with report-only one-shot review")
	}
	return orchestration.Profile{ID: compiled.ID, SourceDigest: sourceDigest, BundleDigest: bundleDigest,
		Instructions: compiled.Instructions, Timeout: timeout, MaxInputBytes: compiled.Limits.MaxInputBytes,
		MaxOutputBytes: compiled.Limits.MaxOutputBytes, MaxTokens: compiled.Limits.MaxTokens,
		MaxConcurrency: compiled.Limits.MaxConcurrency}, nil
}

func oneOfString(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func onlyContainsStrings(values []string, allowed ...string) bool {
	for _, value := range values {
		if !containsString(allowed, value) {
			return false
		}
	}
	return true
}

func (host *orchestrationReviewHost) putBinding(command reviewBindingCommand) (store.ReviewBinding, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.closing {
		return store.ReviewBinding{}, errors.New("review host is closing")
	}
	detail, err := host.profiles.GetRevision(command.ProfileID, command.ProfileSourceDigest, command.ProfileBundleDigest)
	if err != nil || detail.Normalized == nil {
		return store.ReviewBinding{}, errors.New("selected profile revision is unavailable")
	}
	profile, err := sliceCProfile(*detail.Normalized, command.ProfileSourceDigest, command.ProfileBundleDigest)
	if err != nil {
		return store.ReviewBinding{}, err
	}
	if command.TimeoutMS < 1000 || command.TimeoutMS > 120000 ||
		time.Duration(command.TimeoutMS)*time.Millisecond > profile.Timeout {
		return store.ReviewBinding{}, errors.New("review timeout must be between one second and the profile ceiling")
	}
	if command.Effect == "" {
		command.Effect = "report-only"
	}
	if command.Effect != "report-only" && command.Effect != "delegated-first" {
		return store.ReviewBinding{}, errors.New("review effect must be report-only or delegated-first")
	}
	if command.Effect == "report-only" {
		command.ApprovalSubdeadlineMS = 0
		command.AnswerChoicePrompts = false
	} else if command.ApprovalSubdeadlineMS < 250 || command.ApprovalSubdeadlineMS > command.TimeoutMS {
		return store.ReviewBinding{}, errors.New("approval subdeadline must be between 250ms and the review timeout")
	}
	if command.Effect == "report-only" && detail.Normalized.Output.Kind != "review-recommendation" {
		return store.ReviewBinding{}, errors.New("report-only requires a review-recommendation profile")
	}
	if command.Effect == "delegated-first" &&
		(detail.Normalized.Output.Kind != "approval-response" ||
			!containsString(detail.Normalized.Authority, "respond-approval") ||
			!containsString(detail.Normalized.Requirements.Capabilities, "approval-response")) {
		return store.ReviewBinding{}, errors.New("delegated-first requires a profile that requests respond-approval and approval-response")
	}
	if len(command.RuntimeFilter) > 64 || !validRuntimeName(command.RuntimeFilter) {
		return store.ReviewBinding{}, errors.New("runtime filter must be a short exact runtime identifier")
	}
	path, err := infer.NewLocalOllamaPath(command.Endpoint, command.Model,
		profile.MaxInputBytes, profile.MaxOutputBytes, profile.MaxTokens)
	if err != nil {
		return store.ReviewBinding{}, errors.New("review path must name an explicit literal-loopback Ollama endpoint and model")
	}
	now := time.Now().Unix()
	binding := store.ReviewBinding{BindingID: store.ReviewBindingID, State: "enabled", Effect: command.Effect,
		RuntimeFilter: command.RuntimeFilter, ProfileID: profile.ID,
		ProfileSourceDigest: command.ProfileSourceDigest, ProfileBundleDigest: command.ProfileBundleDigest,
		InstructionDigest: orchestration.InstructionDigest(profile.Instructions), RequestPathKind: path.Kind,
		RequestPathDigest: path.Digest, Endpoint: path.Endpoint, Model: path.Model,
		TimeoutMS: command.TimeoutMS, ApprovalSubdeadlineMS: command.ApprovalSubdeadlineMS,
		AnswerChoicePrompts: command.AnswerChoicePrompts, MaxInputBytes: profile.MaxInputBytes,
		MaxOutputBytes: profile.MaxOutputBytes, MaxTokens: profile.MaxTokens,
		MaxConcurrency: profile.MaxConcurrency}
	binding, err = host.ix.PutReviewBinding(binding, command.ExpectedStateToken, now)
	if err != nil {
		return store.ReviewBinding{}, err
	}
	profile.Timeout = time.Duration(binding.TimeoutMS) * time.Millisecond
	profile.DelegatedApproval = binding.Effect == "delegated-first"
	host.runtimeBinding = &reviewRuntimeBinding{record: binding, profile: profile, path: path}
	host.startupProblem = ""
	return binding, nil
}

func (host *orchestrationReviewHost) disableBinding(expectedToken string) (store.ReviewBinding, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.closing {
		return store.ReviewBinding{}, errors.New("review host is closing")
	}
	binding, err := host.ix.DisableReviewBinding(expectedToken, time.Now().Unix())
	if err != nil {
		return store.ReviewBinding{}, err
	}
	host.runtimeBinding = nil
	host.startupProblem = ""
	return binding, nil
}

// offer receives only an already committed lower-layer observation. It has no return
// value by design: upper-layer storage, capacity, profile, or model failures cannot
// change the observation acknowledgement.
func (host *orchestrationReviewHost) offer(envelope observation.Envelope, receipt observation.Receipt) {
	if host == nil || envelope.ActionID == "" || envelope.CollectorID != observation.CollectorPreTool {
		return
	}
	host.mu.RLock()
	if host.closing || host.runtimeBinding == nil || host.runtimeBinding.record.Effect != "report-only" ||
		(host.runtimeBinding.record.RuntimeFilter != "" && host.runtimeBinding.record.RuntimeFilter != envelope.Runtime) {
		host.mu.RUnlock()
		return
	}
	binding := *host.runtimeBinding
	action := actionFromObservation(envelope, receipt)
	actionDigest, err := orchestration.ActionDigest(action)
	if err != nil {
		host.mu.RUnlock()
		return
	}
	invocationID, err := newReviewInvocationID()
	if err != nil {
		host.mu.RUnlock()
		return
	}
	invocation := store.ReviewInvocation{InvocationID: invocationID, ActionID: action.ActionID,
		ActionDigest: actionDigest, ObservationID: action.ObservationID, EventID: action.EventID,
		Runtime: action.Runtime, SessionID: action.SessionID, NativeCallID: action.NativeCallID,
		NativeCallKind: action.NativeCallKind, Tool: action.Tool, BindingID: binding.record.BindingID,
		BindingStateToken: binding.record.StateToken, ProfileID: binding.record.ProfileID,
		ProfileSourceDigest: binding.record.ProfileSourceDigest,
		ProfileBundleDigest: binding.record.ProfileBundleDigest, InstructionDigest: binding.record.InstructionDigest,
		RequestPathKind: binding.path.Kind, RequestPathDigest: binding.path.Digest, Endpoint: binding.path.Endpoint,
		Model: binding.path.Model, TimeoutMS: binding.record.TimeoutMS, MaxInputBytes: binding.path.MaxInputBytes,
		MaxOutputBytes: binding.path.MaxOutputBytes, MaxTokens: binding.path.MaxTokens,
		MaxConcurrency: binding.record.MaxConcurrency, AdmittedAt: time.Now().Unix(),
		TimingClass: "result_not_observed_at_completion"}
	admitted, created, err := host.ix.AdmitReview(invocation)
	// Binding mutation and the cache swap use the matching write lock. Keep the
	// read lock through durable admission so an update or disable cannot return
	// while an observation admitted against its prior binding is still pending.
	host.mu.RUnlock()
	if err != nil || !created || admitted.State != "admitted" {
		return
	}
	select {
	case host.workers <- struct{}{}:
	case <-host.ctx.Done():
		return
	default:
		if host.ix.MarkReviewRunning(admitted.InvocationID, time.Now().Unix()) == nil {
			_ = host.ix.CompleteReview(admitted.InvocationID, store.ReviewCompletion{State: "suppressed",
				CompletedAt: time.Now().Unix(), ErrorClass: "host_capacity",
				Recovery: "Wait for active report reviews to finish; this action will not be replayed."})
		}
		return
	}
	host.mu.Lock()
	if host.closing {
		host.mu.Unlock()
		<-host.workers
		return
	}
	host.wg.Add(1)
	host.mu.Unlock()
	go func() {
		defer host.wg.Done()
		defer func() { <-host.workers }()
		runner := pinnedReviewRunner{backendName: binding.path.Backend, path: binding.path}
		service, serviceErr := orchestration.NewService(runner, reviewLifecycle{host: host})
		if serviceErr == nil {
			_ = service.Execute(host.ctx, admitted.InvocationID, binding.profile, action)
		}
	}()
}

func (host *orchestrationReviewHost) offerApproval(approval Approval, issue func(ApprovalResponder) (approvalResponderCapability, error)) {
	if approval.Status != "pending" || approval.Mode != "ask" || issue == nil {
		return
	}
	host.mu.RLock()
	if host.closing || host.runtimeBinding == nil || host.runtimeBinding.record.Effect != "delegated-first" ||
		(host.runtimeBinding.record.RuntimeFilter != "" && host.runtimeBinding.record.RuntimeFilter != approval.Runtime) {
		host.mu.RUnlock()
		return
	}
	binding := *host.runtimeBinding
	host.mu.RUnlock()
	if len(approval.Prompts) != 0 && !binding.record.AnswerChoicePrompts {
		// This reviewer may not answer questions. Offering it the approval
		// anyway would spend the subdeadline on a response the owner must
		// refuse, so the person gets the whole window instead.
		return
	}
	deadline, err := time.Parse(time.RFC3339, approval.Deadline)
	if err != nil {
		return
	}
	remaining := time.Until(deadline) - 250*time.Millisecond
	subdeadline := time.Duration(binding.record.ApprovalSubdeadlineMS) * time.Millisecond
	if remaining < 250*time.Millisecond {
		return
	}
	if subdeadline > remaining {
		subdeadline = remaining
	}
	responder := ApprovalResponder{Kind: "service", ID: "orchestration:" + binding.record.ProfileID}
	capability, err := issue(responder)
	if err != nil {
		return
	}
	action := actionFromApproval(approval)
	actionDigest, err := orchestration.ActionDigest(action)
	if err != nil {
		approvals.revokeServiceResponder(capability)
		return
	}
	invocationID, err := newReviewInvocationID()
	if err != nil {
		approvals.revokeServiceResponder(capability)
		return
	}
	invocation := store.ReviewInvocation{InvocationID: invocationID, ActionID: action.ActionID,
		ActionDigest: actionDigest, Runtime: action.Runtime, SessionID: action.SessionID,
		NativeCallID: action.NativeCallID, NativeCallKind: action.NativeCallKind, Tool: action.Tool,
		BindingID: binding.record.BindingID, BindingStateToken: binding.record.StateToken,
		ProfileID: binding.record.ProfileID, ProfileSourceDigest: binding.record.ProfileSourceDigest,
		ProfileBundleDigest: binding.record.ProfileBundleDigest, InstructionDigest: binding.record.InstructionDigest,
		RequestPathKind: binding.path.Kind, RequestPathDigest: binding.path.Digest, Endpoint: binding.path.Endpoint,
		Model: binding.path.Model, TimeoutMS: int(subdeadline.Milliseconds()), MaxInputBytes: binding.path.MaxInputBytes,
		MaxOutputBytes: binding.path.MaxOutputBytes, MaxTokens: binding.path.MaxTokens,
		MaxConcurrency: binding.record.MaxConcurrency, AdmittedAt: time.Now().Unix(),
		TimingClass: "result_not_observed_at_completion", ApprovalID: approval.ID}
	admitted, created, err := host.ix.AdmitReview(invocation)
	if err != nil || !created || admitted.State != "admitted" {
		approvals.revokeServiceResponder(capability)
		return
	}
	select {
	case host.workers <- struct{}{}:
	case <-host.ctx.Done():
		approvals.revokeServiceResponder(capability)
		return
	default:
		approvals.revokeServiceResponder(capability)
		if host.ix.MarkReviewRunning(admitted.InvocationID, time.Now().Unix()) == nil {
			_ = host.ix.CompleteReview(admitted.InvocationID, store.ReviewCompletion{State: "suppressed",
				CompletedAt: time.Now().Unix(), ErrorClass: "host_capacity",
				Recovery: "Use the ordinary human approval path; delegated review capacity was unavailable."})
			_ = host.ix.LinkReviewApprovalOutcome(admitted.InvocationID, approval.ID, "", "fallback-human")
		}
		return
	}
	host.mu.Lock()
	if host.closing {
		host.mu.Unlock()
		<-host.workers
		approvals.revokeServiceResponder(capability)
		return
	}
	host.wg.Add(1)
	host.mu.Unlock()
	go func() {
		defer host.wg.Done()
		defer func() { <-host.workers }()
		profile := binding.profile
		profile.Timeout = subdeadline
		runner := pinnedReviewRunner{backendName: binding.path.Backend, path: binding.path}
		lifecycle := delegatedReviewLifecycle{reviewLifecycle: reviewLifecycle{host: host}, approval: approval,
			responder: responder, capability: capability}
		service, serviceErr := orchestration.NewService(runner, lifecycle)
		if serviceErr != nil {
			approvals.revokeServiceResponder(capability)
			return
		}
		_ = service.Execute(host.ctx, admitted.InvocationID, profile, action)
	}()
}

func actionFromApproval(approval Approval) orchestration.Action {
	sessionID := approval.CatalogSessionID
	if sessionID == "" {
		sessionID = approval.NativeSessionID
	}
	if sessionID == "" {
		sessionID = approval.Session
	}
	tool := approval.ToolName
	if tool == "" {
		tool = approval.Rule
	}
	return orchestration.Action{ActionID: "approval:" + approval.ID, Runtime: approval.Runtime,
		SessionID: sessionID, NativeCallID: approval.ToolCallID, NativeCallKind: "approval-tool-call",
		Tool: tool, Command: approval.Command, Content: approval.Summary, ObservedDecision: "ask",
		ToolInputCompleteness: "unavailable",
		Prompts:               approval.Prompts, MaxFreeTextBytes: activeApprovalsConfig().MaxFreeTextBytes}
}

func actionFromObservation(envelope observation.Envelope, receipt observation.Receipt) orchestration.Action {
	resources := make([]orchestration.ResourceFact, 0, len(envelope.ResourceClaims))
	for _, claim := range envelope.ResourceClaims {
		resources = append(resources, orchestration.ResourceFact{Kind: claim.Kind, Identity: claim.Identity,
			RawIdentity: claim.RawIdentity, Operation: claim.Operation, SourceField: claim.SourceField,
			Completeness: claim.Completeness})
	}
	return orchestration.Action{ActionID: envelope.ActionID, ObservationID: receipt.ObservationID,
		EventID: receipt.EventID, Runtime: envelope.Runtime, SessionID: envelope.SessionID,
		NativeCallID: envelope.NativeCallID, NativeCallKind: envelope.NativeCallKind, Tool: envelope.Tool,
		Cwd: envelope.Cwd, Command: envelope.Command, Content: envelope.Content, FilePath: envelope.FilePath,
		FilePaths: append([]string(nil), envelope.FilePaths...), URL: envelope.URL, Skill: envelope.Skill,
		ObservedDecision: envelope.Decision, ToolInput: append([]byte(nil), envelope.ToolInput...),
		ToolInputBytes:  envelope.ToolInputBytes,
		ToolInputDigest: envelope.ToolInputDigest, ToolInputCompleteness: envelope.ToolInputCompleteness,
		Resources: resources}
}

type pinnedReviewRunner struct {
	backendName string
	path        infer.RequestPath
}

func (runner pinnedReviewRunner) Run(ctx context.Context, request orchestration.RunRequest) (orchestration.RunResponse, error) {
	backend, err := infer.RequireBackend(runner.backendName)
	if err != nil {
		return orchestration.RunResponse{}, &orchestration.RunError{Kind: "unavailable"}
	}
	messages := make([]infer.Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		messages = append(messages, infer.Message{Role: message.Role, Content: message.Content})
	}
	response, err := backend.Run(ctx, infer.Request{Path: runner.path, Messages: messages, Schema: request.Schema})
	out := orchestration.RunResponse{Content: response.Content, RequestBytes: response.RequestBytes,
		ResponseBytes: response.ResponseBytes, PromptTokens: response.PromptTokens,
		CompletionTokens: response.CompletionTokens}
	if err != nil {
		kind := "unavailable"
		switch infer.Kind(err) {
		case infer.ErrorTimeout:
			kind = "timed_out"
		case infer.ErrorMalformed:
			kind = "malformed"
		}
		return out, &orchestration.RunError{Kind: kind}
	}
	return out, nil
}

type reviewLifecycle struct{ host *orchestrationReviewHost }

func (lifecycle reviewLifecycle) MarkRunning(invocationID string, startedAt int64) error {
	return lifecycle.host.ix.MarkReviewRunning(invocationID, startedAt)
}

func (lifecycle reviewLifecycle) Complete(invocationID string, completion orchestration.Completion) error {
	action, message, citations := "", "", []string{}
	selections := ""
	if completion.Claim != nil {
		action, message = completion.Claim.Action, completion.Claim.Message
		citations = append(citations, completion.Claim.Citations...)
		if len(completion.Claim.Selections) != 0 {
			if encoded, err := json.Marshal(completion.Claim.Selections); err == nil {
				selections = string(encoded)
			}
		}
	}
	lifecycle.host.mu.RLock()
	closing := lifecycle.host.closing
	lifecycle.host.mu.RUnlock()
	if closing {
		completion.State, completion.ErrorClass = "unknown", "restart_unknown"
		completion.Recovery = "The daemon stopped during review; this request was not replayed."
		action, message, citations, selections = "", "", []string{}, ""
	}
	return lifecycle.host.ix.CompleteReview(invocationID, store.ReviewCompletion{State: completion.State,
		Action: action, Message: message, Citations: citations, SelectionsJSON: selections,
		CompletedAt: completion.CompletedAt,
		DurationMS:  completion.DurationMS, RequestBytes: completion.RequestBytes,
		ResponseBytes: completion.ResponseBytes, PromptTokens: completion.PromptTokens,
		CompletionTokens: completion.CompletionTokens, ErrorClass: completion.ErrorClass,
		Recovery: completion.Recovery})
}

type delegatedReviewLifecycle struct {
	reviewLifecycle
	approval   Approval
	responder  ApprovalResponder
	capability approvalResponderCapability
}

func (lifecycle delegatedReviewLifecycle) Complete(invocationID string, completion orchestration.Completion) error {
	if err := lifecycle.reviewLifecycle.Complete(invocationID, completion); err != nil {
		approvals.revokeServiceResponder(lifecycle.capability)
		return err
	}
	lifecycle.host.mu.RLock()
	closing := lifecycle.host.closing
	lifecycle.host.mu.RUnlock()
	if closing || completion.Claim == nil || completion.Claim.Action == "abstain" {
		approvals.revokeServiceResponder(lifecycle.capability)
		outcome := "fallback-human"
		if closing {
			outcome = "restart-unknown"
		}
		return lifecycle.host.ix.LinkReviewApprovalOutcome(invocationID, lifecycle.approval.ID, "", outcome)
	}
	responseID := approvals.newResponseID()
	outcome := approvals.respond(approvalResponseCommand{approvalID: lifecycle.approval.ID,
		responseID: responseID, responder: lifecycle.responder, decision: completion.Claim.Action,
		reason: completion.Claim.Message, selections: completion.Claim.Selections,
		submitted: approvals.now(), capability: lifecycle.capability})
	return lifecycle.host.ix.LinkReviewApprovalOutcome(invocationID, lifecycle.approval.ID,
		responseID, string(outcome.kind))
}

func newReviewInvocationID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "rev_" + hex.EncodeToString(value), nil
}
