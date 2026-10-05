package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"crossing-guard/internal/daemon"
)

// The handoff verbs (team rest-of-release plan §6.9): send, list, show, decline,
// withdraw, close, and clean. The daemon does the work; these verbs ask it — except
// clean, which edits a checkout's files and needs no daemon. There is no open verb:
// opening a handoff is the console's act (OD-19), so no terminal command, and no
// session started from one, ever picks a handoff up.

const handoffUsage = `usage:
  crossing-guard handoff send --session <runtime>/<id> (--to <member> | --local)
        [--title T] [--remaining "item"]... [--body-file F] [--include-conversation]
      send a session to a teammate (a display name or user id from the member
      directory), or write a local handoff. Prints what leaves before it is sent.
  crossing-guard handoff list
      the handoffs this device received and sent.
  crossing-guard handoff show <id>
      one handoff; on a device of its recipient, with its text.
  crossing-guard handoff decline|withdraw|close <id>
      decline a received handoff, withdraw one you sent, or close one that was started.
  crossing-guard handoff clean <dir>
      remove the handoff file and marker blocks earlier versions wrote into a checkout.
`

// handoffVerbs is the dispatcher's table. It has no "open" on purpose (OD-19).
var handoffVerbs = map[string]func(args []string) int{
	"send":     handoffSend,
	"list":     handoffList,
	"show":     handoffShow,
	"decline":  func(args []string) int { return handoffAct("decline", args) },
	"withdraw": func(args []string) int { return handoffAct("withdraw", args) },
	"close":    func(args []string) int { return handoffAct("close", args) },
	"clean":    handoffClean,
}

func handoffCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, handoffUsage)
		return 2
	}
	verb, ok := handoffVerbs[args[0]]
	if !ok {
		fmt.Fprint(os.Stderr, handoffUsage)
		return 2
	}
	return verb(args[1:])
}

// repeated collects a flag given more than once.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, "; ") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// handoffSendOptions is `handoff send` as parsed.
type handoffSendOptions struct {
	runtime, sessionID, to, title, bodyFile string
	remaining                               repeated
	includeConversation, local              bool
}

func parseHandoffSend(args []string) (handoffSendOptions, error) {
	var o handoffSendOptions
	var session string
	fs := flag.NewFlagSet("handoff send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&session, "session", "", "the session to hand off, as <runtime>/<id>")
	fs.StringVar(&o.to, "to", "", "the recipient: a display name or user id from the member directory")
	fs.StringVar(&o.title, "title", "", "the title (default: from the session's first request)")
	fs.Var(&o.remaining, "remaining", "one item of what remains; repeat for more")
	fs.StringVar(&o.bodyFile, "body-file", "", "a file holding the text to send (default: the mechanical extract)")
	fs.BoolVar(&o.includeConversation, "include-conversation", false, "include the session's last turns")
	fs.BoolVar(&o.local, "local", false, "write a local handoff on this device; no server, no recipient")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	runtime, id, found := strings.Cut(session, "/")
	if !found || runtime == "" || id == "" {
		return o, fmt.Errorf("--session must be <runtime>/<id>")
	}
	o.runtime, o.sessionID = runtime, id
	if o.local == (o.to != "") {
		return o, fmt.Errorf("give exactly one of --to <member> and --local")
	}
	return o, nil
}

// handoffSendBody is the send route's request; the same value is posted for the
// preview and, with the preview's id, time and hash, for the send.
type handoffSendBody struct {
	Runtime             string   `json:"runtime"`
	SessionID           string   `json:"session_id"`
	To                  string   `json:"to"`
	Local               bool     `json:"local"`
	Title               string   `json:"title"`
	BodyMarkdown        string   `json:"body_markdown"`
	Remaining           []string `json:"remaining"`
	IncludeConversation bool     `json:"include_conversation"`
	Preview             bool     `json:"preview"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	WireHash            string   `json:"wire_hash"`
}

// handoffShown is what the send route answers: the content that leaves.
type handoffShown struct {
	ID           string   `json:"id"`
	CreatedAt    string   `json:"created_at"`
	WireHash     string   `json:"wire_hash"`
	Title        string   `json:"title"`
	Remaining    []string `json:"remaining"`
	BodyMarkdown string   `json:"body_markdown"`
	Bytes        int      `json:"bytes"`
	State        string   `json:"state"`
	Recipient    *struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
	} `json:"recipient"`
	Conversation *struct {
		TurnCount int  `json:"turn_count"`
		Bytes     int  `json:"bytes"`
		Truncated bool `json:"truncated"`
	} `json:"conversation"`
	Checks struct {
		RedactionCount int `json:"redaction_count"`
		AbsolutePaths  int `json:"absolute_paths"`
	} `json:"checks"`
}

func locateOrExplain() (daemon.Location, bool) {
	loc, err := daemon.LocateConsole()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no local daemon to ask:", err)
		return loc, false
	}
	return loc, true
}

// handoffSend drafts from the session, previews, prints exactly what leaves, and sends
// the previewed document. The person's own flags replace the draft's parts.
func handoffSend(args []string) int {
	o, err := parseHandoffSend(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprint(os.Stderr, handoffUsage)
		return 2
	}
	loc, ok := locateOrExplain()
	if !ok {
		return 1
	}
	var draft struct {
		Title     string   `json:"title"`
		Markdown  string   `json:"markdown"`
		Remaining []string `json:"remaining"`
	}
	query := "/api/handoff/generate?runtime=" + url.QueryEscape(o.runtime) + "&id=" + url.QueryEscape(o.sessionID)
	if err := loc.GetJSON(query, &draft); err != nil {
		fmt.Fprintln(os.Stderr, "the session could not be read:", linkError(err))
		return 1
	}
	body := handoffSendBody{Runtime: o.runtime, SessionID: o.sessionID, To: o.to, Local: o.local, Title: draft.Title,
		BodyMarkdown: draft.Markdown, Remaining: draft.Remaining, IncludeConversation: o.includeConversation, Preview: true}
	if o.title != "" {
		body.Title = o.title
	}
	if len(o.remaining) > 0 {
		body.Remaining = o.remaining
	}
	if o.bodyFile != "" {
		raw, err := os.ReadFile(o.bodyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "the body file could not be read:", err)
			return 1
		}
		body.BodyMarkdown = string(raw)
	}
	var shown handoffShown
	if err := loc.PostJSON("/api/team/handoffs/send", body, &shown); err != nil {
		fmt.Fprintln(os.Stderr, "the handoff was refused:", linkError(err))
		return 1
	}
	printHandoffShown(os.Stdout, shown, o.local)
	body.Preview, body.ID, body.CreatedAt, body.WireHash = false, shown.ID, shown.CreatedAt, shown.WireHash
	var sent handoffShown
	if err := loc.PostJSON("/api/team/handoffs/send", body, &sent); err != nil {
		fmt.Fprintln(os.Stderr, "the handoff was not sent:", linkError(err))
		return 1
	}
	fmt.Printf("\n%s  %s\n", sent.ID, sent.State)
	return 0
}

// printHandoffShown prints the content that leaves, as the send sheet shows it.
func printHandoffShown(w io.Writer, shown handoffShown, local bool) {
	switch {
	case local:
		fmt.Fprintln(w, "A local handoff: it stays on this device.")
	case shown.Recipient != nil:
		fmt.Fprintf(w, "To %s (%s). The recipient chooses the runtime; this text may be sent to whichever model provider their device uses.\n",
			shown.Recipient.DisplayName, shown.Recipient.UserID)
	}
	fmt.Fprintf(w, "\nTitle: %s\n", shown.Title)
	if len(shown.Remaining) > 0 {
		fmt.Fprintln(w, "\nRemaining:")
		for _, item := range shown.Remaining {
			fmt.Fprintf(w, "  - %s\n", item)
		}
	}
	fmt.Fprintf(w, "\n%s\n", shown.BodyMarkdown)
	if shown.Conversation != nil {
		fmt.Fprintf(w, "Conversation excerpt: %d turns, %d bytes", shown.Conversation.TurnCount, shown.Conversation.Bytes)
		if shown.Conversation.Truncated {
			fmt.Fprint(w, " (cut to fit)")
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%d bytes leave; %d secret pattern matches became markers; %d paths became \"[absolute path]\".\n",
		shown.Bytes, shown.Checks.RedactionCount, shown.Checks.AbsolutePaths)
}

// handoffRow is one listed handoff.
type handoffRow struct {
	ID                string `json:"id"`
	Local             bool   `json:"local"`
	FromAnotherDevice bool   `json:"from_another_device"`
	Title             string `json:"title"`
	State             string `json:"state"`
	CreatedAt         string `json:"created_at"`
	RefusalCode       string `json:"refusal_code"`
	ReceiptCode       string `json:"receipt_code"`
	HasText           bool   `json:"has_text"`
	EverHeld          bool   `json:"ever_held"`
	LinkEnded         bool   `json:"link_ended"`
	Peer              struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
	} `json:"peer"`
	Offers []string `json:"offers"`
}

func (r handoffRow) line() string {
	title := r.Title
	switch {
	case title != "":
	case r.FromAnotherDevice:
		title = "(sent from another of your devices)"
	case !r.EverHeld:
		title = "(" + r.State + " before it reached this device)"
	default:
		title = "(no text held)"
	}
	who := r.Peer.DisplayName
	if who == "" {
		who = r.Peer.UserID
	}
	if r.Local {
		who = "this device"
	}
	state := r.State
	if r.RefusalCode != "" {
		state += " (" + r.RefusalCode + ")"
	}
	if r.LinkEnded {
		state += ", link ended"
	}
	return fmt.Sprintf("%s  %-10s  %-20s  %s  %s", r.ID, state, who, r.CreatedAt, title)
}

func handoffList(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, handoffUsage)
		return 2
	}
	loc, ok := locateOrExplain()
	if !ok {
		return 1
	}
	var out struct {
		Received  []handoffRow `json:"received"`
		Sent      []handoffRow `json:"sent"`
		Transport struct {
			ServerCarriesHandoffs bool `json:"server_carries_handoffs"`
		} `json:"transport"`
		Linked bool `json:"linked"`
	}
	if err := loc.GetJSON("/api/team/handoffs", &out); err != nil {
		fmt.Fprintln(os.Stderr, "the handoffs could not be read:", linkError(err))
		return 1
	}
	if out.Linked && !out.Transport.ServerCarriesHandoffs {
		fmt.Println("this team server does not carry handoffs; what is queued leaves when it does")
	}
	fmt.Println("Received")
	for _, r := range out.Received {
		fmt.Println("  " + r.line())
	}
	fmt.Println("Sent")
	for _, r := range out.Sent {
		fmt.Println("  " + r.line())
	}
	return 0
}

// handoffDetail is the detail route's answer as the verbs read it.
type handoffDetail struct {
	Handoff  handoffRow `json:"handoff"`
	Document *struct {
		Title        string   `json:"title"`
		BodyMarkdown string   `json:"body_markdown"`
		Remaining    []string `json:"remaining"`
		Conversation *struct {
			Turns []struct {
				Seq  int    `json:"seq"`
				Role string `json:"role"`
				Text string `json:"text"`
			} `json:"turns"`
			Truncated bool `json:"truncated"`
		} `json:"conversation"`
	} `json:"document"`
}

func handoffShow(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, handoffUsage)
		return 2
	}
	loc, ok := locateOrExplain()
	if !ok {
		return 1
	}
	var out handoffDetail
	if err := loc.GetJSON("/api/team/handoffs/"+url.PathEscape(args[0]), &out); err != nil {
		fmt.Fprintln(os.Stderr, "the handoff could not be read:", linkError(err))
		return 1
	}
	fmt.Println(out.Handoff.line())
	if len(out.Handoff.Offers) > 0 {
		fmt.Println("offers: " + strings.Join(out.Handoff.Offers, ", "))
	}
	if out.Document == nil {
		fmt.Println("\nThis device holds no text for this handoff.")
		return 0
	}
	if len(out.Document.Remaining) > 0 {
		fmt.Println("\nRemaining:")
		for _, item := range out.Document.Remaining {
			fmt.Println("  - " + item)
		}
	}
	fmt.Printf("\n%s\n", out.Document.BodyMarkdown)
	if c := out.Document.Conversation; c != nil {
		fmt.Printf("\nConversation excerpt (%d turns):\n", len(c.Turns))
		for _, turn := range c.Turns {
			fmt.Printf("\n[%s #%d]\n%s\n", turn.Role, turn.Seq, turn.Text)
		}
	}
	return 0
}

func handoffAct(action string, args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, handoffUsage)
		return 2
	}
	loc, ok := locateOrExplain()
	if !ok {
		return 1
	}
	var out handoffDetail
	if err := loc.PostJSON("/api/team/handoffs/"+url.PathEscape(args[0])+"/"+action, struct{}{}, &out); err != nil {
		fmt.Fprintln(os.Stderr, action+" refused:", linkError(err))
		return 1
	}
	fmt.Printf("%s  %s\n", out.Handoff.ID, out.Handoff.State)
	return 0
}

// handoffClean removes what earlier versions wrote into a checkout. A damaged marker
// pair is left untouched and reported, and the verb exits 1 so a script notices.
func handoffClean(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, handoffUsage)
		return 2
	}
	removed, err := daemon.CleanCheckoutHandoff(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "clean:", err)
		return 1
	}
	if removed.File != "" {
		fmt.Println("removed", removed.File)
	}
	for _, path := range removed.Blocks {
		fmt.Println("removed the handoff block from", path)
	}
	for _, path := range removed.Damaged {
		fmt.Fprintf(os.Stderr, "%s: the handoff markers are damaged (not one start followed by one end); left untouched — remove them by hand\n", path)
	}
	if removed.File == "" && len(removed.Blocks) == 0 && len(removed.Damaged) == 0 {
		fmt.Println("nothing to clean in", args[0])
	}
	if len(removed.Damaged) > 0 {
		return 1
	}
	return 0
}

// handoffLeftoverHealth is doctor's handoff section: the checkouts this device has
// seen that still hold a handoff file or marker block an earlier version wrote.
type handoffLeftoverHealth struct {
	Checkouts []daemon.HandoffLeftover `json:"checkouts"`
}

// handoffLeftoverProbe asks the running daemon which seen checkouts still hold them.
// nil when no daemon answers or none does.
func handoffLeftoverProbe() *handoffLeftoverHealth {
	loc, err := daemon.LocateConsole()
	if err != nil {
		return nil
	}
	var out struct {
		Leftovers []daemon.HandoffLeftover `json:"checkout_leftovers"`
	}
	if err := loc.GetJSON("/api/team/handoffs?leftovers=1", &out); err != nil || len(out.Leftovers) == 0 {
		return nil
	}
	return &handoffLeftoverHealth{Checkouts: out.Leftovers}
}

// lines words the section for doctor's text report.
func (h *handoffLeftoverHealth) lines() []string {
	var out []string
	for _, c := range h.Checkouts {
		out = append(out, fmt.Sprintf("%s still holds a handoff written by an earlier version; run: crossing-guard handoff clean %s", c.Dir, c.Dir))
	}
	return out
}
