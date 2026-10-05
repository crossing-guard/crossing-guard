package main

import (
	"fmt"
	"io"
	"os"

	"crossing-guard/internal/daemon"
)

const layersUsage = "usage: crossing-guard layers [--adopt <scope>] [--unadopt <scope>] [--repin <fingerprint>]"

// layersOffer is one verified bundle as the daemon offers it.
type layersOffer struct {
	ID              string `json:"id"`
	Scope           string `json:"scope"`
	Revision        int64  `json:"revision"`
	ExpiresAt       string `json:"expires_at"`
	FailureMode     string `json:"failure_mode"`
	Adopted         bool   `json:"adopted"`
	Status          string `json:"status"`
	AdoptedRevision int64  `json:"adopted_revision"`
	StateToken      string `json:"state_token"`
	Documents       []struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		State     string `json:"state"`
		Reason    string `json:"reason"`
		ProfileID string `json:"profile_id"`
	} `json:"documents"`
	Changes []struct {
		Label string `json:"label"`
		From  string `json:"from"`
		To    string `json:"to"`
	} `json:"changes"`
	Rules []struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	} `json:"rules"`
}

// layersState is the part of GET /api/team/layers this verb prints.
type layersState struct {
	Available []layersOffer `json:"available"`
	Unusable  []struct {
		ID              string `json:"id"`
		Scope           string `json:"scope"`
		Revision        int64  `json:"revision"`
		StillOnRevision int64  `json:"still_on_revision"`
		Reason          string `json:"reason"`
	} `json:"unusable"`
	AdoptedBundles []struct {
		OrganizationID   string `json:"organization_id"`
		OrganizationName string `json:"organization_name"`
		Scope            string `json:"scope"`
		BundleID         string `json:"bundle_id"`
		Revision         int64  `json:"revision"`
		ExpiresAt        string `json:"expires_at"`
		FailureMode      string `json:"failure_mode"`
		Linked           bool   `json:"linked"`
		Expired          bool   `json:"expired"`
		Blocking         bool   `json:"blocking"`
	} `json:"adopted_bundles"`
	KeyMismatches []struct {
		PinnedFingerprint    string `json:"pinned_fingerprint"`
		PresentedFingerprint string `json:"presented_fingerprint"`
		PresentedKeyID       string `json:"presented_key_id"`
	} `json:"key_mismatches"`
	PinnedKey struct {
		KeyID       string `json:"key_id"`
		Fingerprint string `json:"fingerprint"`
	} `json:"pinned_org_key"`
	Reasons []string `json:"reasons"`
}

// layersCmd lists the team bundles this device verified and adopted, and is the
// CLI form of the explicit adopt, un-adopt and re-pin actions (team plan §5.16.3;
// rest-of-release plan §4.1 decision 6, §4.3): the daemon does the work; this verb
// asks it and prints what the daemon answered.
func layersCmd(args []string) {
	adoptScope, unadoptScope, repin := "", "", ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--adopt" && i+1 < len(args):
			adoptScope = args[i+1]
			i++
		case args[i] == "--unadopt" && i+1 < len(args):
			unadoptScope = args[i+1]
			i++
		case args[i] == "--repin" && i+1 < len(args):
			repin = args[i+1]
			i++
		default:
			fmt.Fprintln(os.Stderr, layersUsage)
			os.Exit(2)
		}
	}
	loc, err := daemon.LocateConsole()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no local daemon to ask:", err)
		os.Exit(1)
	}
	switch {
	case repin != "":
		layersRepin(loc, repin)
	case unadoptScope != "":
		layersUnadopt(loc, unadoptScope)
	case adoptScope != "":
		layersAdopt(loc, adoptScope)
	default:
		var st layersState
		if err := loc.GetJSON("/api/team/layers", &st); err != nil {
			fmt.Fprintln(os.Stderr, "the layers could not be read:", linkError(err))
			os.Exit(1)
		}
		printLayers(os.Stdout, st)
	}
}

// layersRepin is `layers --repin <fingerprint>`: the same act as Trust new key… in
// the console. The fingerprint is the PRESENTED key's, compared by a person with the
// team console's Policy page.
func layersRepin(loc daemon.Location, fingerprint string) {
	var out struct {
		PinnedKey struct {
			KeyID       string `json:"key_id"`
			Fingerprint string `json:"fingerprint"`
		} `json:"pinned_org_key"`
		ReplacedKeyID       string `json:"replaced_key_id"`
		ReplacedFingerprint string `json:"replaced_fingerprint"`
	}
	body := map[string]string{"fingerprint": fingerprint, "surface": "command"}
	if err := loc.PostJSON("/api/team/org-key/repin", body, &out); err != nil {
		fmt.Fprintln(os.Stderr, "re-pin refused:", linkError(err))
		os.Exit(1)
	}
	fmt.Printf("now trusting organization key %s (%s)\n", out.PinnedKey.KeyID, out.PinnedKey.Fingerprint)
	fmt.Printf("it replaces %s (%s); what this device adopted keeps working\n", out.ReplacedKeyID, out.ReplacedFingerprint)
}

func layersUnadopt(loc daemon.Location, scope string) {
	var out struct {
		Unadopted  string `json:"unadopted"`
		BundleID   string `json:"bundle_id"`
		Revision   int64  `json:"revision"`
		WasAdopted bool   `json:"was_adopted"`
		PlacesOff  struct {
			Managed []string `json:"managed"`
			Review  bool     `json:"review"`
		} `json:"places_off"`
		Offers []layersOffer `json:"offers"`
	}
	body := map[string]string{"scope": scope, "surface": "command"}
	if err := loc.PostJSON("/api/team/layers/unadopt", body, &out); err != nil {
		fmt.Fprintln(os.Stderr, "un-adopt refused:", linkError(err))
		os.Exit(1)
	}
	if !out.WasAdopted {
		fmt.Printf("nothing was adopted for %s\n", out.Unadopted)
		return
	}
	off := len(out.PlacesOff.Managed)
	if out.PlacesOff.Review {
		off++
	}
	fmt.Printf("un-adopted %s (bundle %s, revision %d); its rules no longer apply and %d of its places were turned off\n",
		out.Unadopted, out.BundleID, out.Revision, off)
	for _, offer := range out.Offers {
		printOffer(os.Stdout, offer)
	}
}

func layersAdopt(loc daemon.Location, scope string) {
	var st layersState
	if err := loc.GetJSON("/api/team/layers", &st); err != nil {
		fmt.Fprintln(os.Stderr, "the layers could not be read:", linkError(err))
		os.Exit(1)
	}
	// The newest verified revision for the scope is the one to adopt: after a newer
	// revision arrives, --adopt must name it, not the one it replaces.
	var chosen *layersOffer
	for index := range st.Available {
		offer := &st.Available[index]
		if offer.Scope == scope && (chosen == nil || offer.Revision > chosen.Revision) {
			chosen = offer
		}
	}
	if chosen == nil {
		fmt.Fprintf(os.Stderr, "no verified bundle offers %s; run crossing-guard layers to see what is offered and why\n", scope)
		os.Exit(1)
	}
	var out struct {
		BundleID string      `json:"bundle_id"`
		Revision int64       `json:"revision"`
		Offer    layersOffer `json:"offer"`
	}
	body := map[string]string{"scope": scope, "digest": chosen.ID, "state_token": chosen.StateToken, "surface": "command"}
	if err := loc.PostJSON("/api/team/layers/adopt", body, &out); err != nil {
		fmt.Fprintln(os.Stderr, "adopt refused:", linkError(err))
		os.Exit(1)
	}
	fmt.Printf("adopted %s revision %d (bundle %s); it applies until expiry or un-adopt\n", out.Offer.Scope, out.Revision, out.BundleID)
	printOffer(os.Stdout, out.Offer)
}

// printLayers prints the pin, every adopted bundle, every offer with each document's
// state, and what cannot be used and why.
func printLayers(w io.Writer, st layersState) {
	if st.PinnedKey.KeyID != "" {
		fmt.Fprintf(w, "organization key %s (%s)\n", st.PinnedKey.KeyID, st.PinnedKey.Fingerprint)
	}
	for _, mismatch := range st.KeyMismatches {
		fmt.Fprintf(w, "new key presented: %s (%s); pinned: %s\n", mismatch.PresentedKeyID, mismatch.PresentedFingerprint, mismatch.PinnedFingerprint)
		fmt.Fprintf(w, "  compare it with the team console's Policy page, then: crossing-guard layers --repin %s\n", mismatch.PresentedFingerprint)
	}
	for _, adopted := range st.AdoptedBundles {
		owner := adopted.OrganizationName
		if owner == "" {
			owner = adopted.OrganizationID
		}
		state := "in force"
		switch {
		case adopted.Blocking:
			state = "expired — blocking until renewed or un-adopted"
		case adopted.Expired:
			state = "expired — its rules no longer apply and its agents' places are held"
		}
		fmt.Fprintf(w, "adopted %s from %s: bundle %s revision %d, expires %s, %s (%s)\n",
			adopted.Scope, owner, adopted.BundleID, adopted.Revision, adopted.ExpiresAt, adopted.FailureMode, state)
	}
	if len(st.Available) == 0 && len(st.Unusable) == 0 && len(st.AdoptedBundles) == 0 {
		fmt.Fprintln(w, "no team bundles available")
	}
	for _, offer := range st.Available {
		printOffer(w, offer)
	}
	for _, unusable := range st.Unusable {
		fmt.Fprintf(w, "%s revision %d can't be used", unusable.Scope, unusable.Revision)
		if unusable.StillOnRevision > 0 {
			fmt.Fprintf(w, " · still on revision %d", unusable.StillOnRevision)
		}
		fmt.Fprintf(w, " (bundle %s): %s\n", unusable.ID, unusable.Reason)
	}
	for _, reason := range st.Reasons {
		fmt.Fprintln(w, "note: "+reason)
	}
}

// printOffer prints one offer: its bundle id and revision, where it stands, each
// changed signed field, and each document with its state.
func printOffer(w io.Writer, offer layersOffer) {
	fmt.Fprintf(w, "%s: bundle %s revision %d — %s", offer.Scope, offer.ID, offer.Revision, offer.Status)
	if offer.AdoptedRevision > 0 {
		fmt.Fprintf(w, " (revision %d is adopted)", offer.AdoptedRevision)
	}
	fmt.Fprintf(w, ", expires %s, %s\n", offer.ExpiresAt, offer.FailureMode)
	for _, change := range offer.Changes {
		fmt.Fprintf(w, "  %s: %s → %s\n", change.Label, orNone(change.From), orNone(change.To))
	}
	for _, document := range offer.Documents {
		label := document.Name
		if document.ProfileID != "" {
			label = "agent " + document.ProfileID
		}
		fmt.Fprintf(w, "  %s %s: %s", document.Kind, label, document.State)
		if document.Reason != "" {
			fmt.Fprintf(w, " (%s)", document.Reason)
		}
		fmt.Fprintln(w)
	}
	for _, rule := range offer.Rules {
		fmt.Fprintf(w, "  rule %s: %s\n", rule.ID, rule.Action)
	}
}
