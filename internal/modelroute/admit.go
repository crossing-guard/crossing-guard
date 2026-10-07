package modelroute

import (
	"net/url"
	"strconv"

	"crossing-guard/engine"
	"crossing-guard/infer"
)

// Route admission (team rest-of-release plan §5.4 point 3, OD-25). Whether a route may
// receive an agent's data is the owner's configuration, evaluated against verified
// facts; no rule of that kind is compiled in and none ships. Admit is the ONE evaluator
// of rules that read a route: fact. Neither hook tier holds such a rule
// (engine.StaticTier and the daemon's stateful selector exclude it), and a scan test
// (internal/guardcli TestStaticDecideIsTheOnlyStaticEvaluator) names this function as
// the recorded owner of the evaluator call below.

// AdmissionRefusedCode is the typed code a refused bind or run start carries.
const AdmissionRefusedCode = "admission_refused"

// AdmissionFacts are the verified facts admission decides over (plan §5.4's table).
// Local is the runtime adapter's own claim about the route — never an egress proof.
// DestinationClass and DestinationHost are set for an inference route only, from the
// first hop the loopback check accepts.
type AdmissionFacts struct {
	RouteID          string
	Family           string
	Runtime          string
	Model            string
	Local            bool
	DestinationClass string
	DestinationHost  string
	ProfileLocality  string
	ProjectRoot      string
}

// Admission is the answer for one route on one place.
type Admission struct {
	// Allowed is false when a deny or an ask rule fired. An ask is a refusal here:
	// a bind or a run start has nobody to ask in the moment.
	Allowed bool `json:"allowed"`
	// Action is the winning rule's authored action: deny, ask or observe; "" when no
	// route rule fired.
	Action  string `json:"action,omitempty"`
	Rule    string `json:"rule,omitempty"`
	Message string `json:"message,omitempty"`
	// Fired names every route rule that fired, in document order. An observe rule that
	// fired on an allowed admission is recorded here and nowhere stops anything.
	Fired []string `json:"fired"`
}

// AdmissionRefused is the typed error of a bind or a run start admission refused.
type AdmissionRefused struct {
	Admission Admission
}

func (refused *AdmissionRefused) Error() string {
	text := AdmissionRefusedCode + ": rule " + refused.Admission.Rule + " does not admit this model route"
	if refused.Admission.Message != "" {
		text += ": " + refused.Admission.Message
	}
	return text
}

// Code is the typed code of the refusal.
func (refused *AdmissionRefused) Code() string { return AdmissionRefusedCode }

// FactsFor builds the admission facts of one route for one place. local is the runtime
// adapter's claim for a runtime-model route; for an inference route it is derived here
// from the loopback check and local is ignored.
func FactsFor(route Route, local bool, profileLocality, projectRoot string) AdmissionFacts {
	facts := AdmissionFacts{RouteID: route.RouteID, Family: route.Family, Runtime: route.Fields.Runtime,
		Model: route.Fields.Model, Local: local, ProfileLocality: profileLocality, ProjectRoot: projectRoot}
	if route.Family == FamilyInference {
		facts.Local = false
		if canonical, err := infer.CanonicalLoopbackEndpoint(route.Fields.Endpoint); err == nil {
			facts.Local, facts.DestinationClass = true, "loopback"
			if parsed, parseErr := url.Parse(canonical); parseErr == nil {
				facts.DestinationHost = parsed.Hostname()
			}
		}
	}
	return facts
}

// Tags renders the facts as the tag set a route rule is judged over. A fact with no
// value is left out, so a term on it does not match — and a rule cannot mix route:
// with anything this list does not carry (engine.ValidateRouteRules).
func (facts AdmissionFacts) Tags() []engine.Tag {
	tags := []engine.Tag{
		{Key: engine.RouteFactPrefix + "id", Value: facts.RouteID},
		{Key: engine.RouteFactPrefix + "family", Value: facts.Family},
		{Key: engine.RouteFactPrefix + "model", Value: facts.Model},
		{Key: engine.RouteFactPrefix + "local", Value: strconv.FormatBool(facts.Local)},
	}
	optional := []engine.Tag{
		{Key: engine.RouteFactPrefix + "runtime", Value: facts.Runtime},
		{Key: engine.RouteFactPrefix + "destination-class", Value: facts.DestinationClass},
		{Key: engine.RouteFactPrefix + "destination-host", Value: facts.DestinationHost},
		{Key: engine.ProfileLocalityFactKey, Value: facts.ProfileLocality},
		{Key: engine.ProjectRootFactKey, Value: facts.ProjectRoot},
	}
	for _, tag := range optional {
		if tag.Value != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// Admit decides one route on one place against the selected rulebook, keeping ONLY the
// rules that read a route: fact (engine.RouteTier). With no such rule, or none firing,
// the route is admitted. A deny or an ask rule refuses; an observe (or any other
// non-gating) rule that fired is recorded in Fired and the route is admitted.
func Admit(policy *engine.Policy, facts AdmissionFacts) Admission {
	admission := Admission{Allowed: true, Fired: []string{}}
	routeRules := engine.RouteTier(policy)
	if routeRules == nil || len(routeRules.Rules) == 0 {
		return admission
	}
	decision, seen := engine.DecideSeeing(facts.Tags(), routeRules, nil)
	for _, rule := range seen.Fired {
		admission.Fired = append(admission.Fired, rule.ID)
	}
	if len(seen.Fired) == 0 {
		return admission
	}
	admission.Rule, admission.Message = decision.Rule, decision.Message
	admission.Action = "observe"
	for _, rule := range seen.Fired {
		if rule.ID == decision.Rule {
			if rule.Action != "" {
				admission.Action = rule.Action
			} else if rule.Gates() {
				// A rule authored with the legacy mode ladder carries no action word.
				admission.Action = "deny"
			}
			admission.Allowed = !rule.Gates()
		}
	}
	return admission
}

// Refusal returns the typed error of a refused admission, or nil when it was admitted.
func (admission Admission) Refusal() error {
	if admission.Allowed {
		return nil
	}
	return &AdmissionRefused{Admission: admission}
}
