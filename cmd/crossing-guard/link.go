package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"crossing-guard/internal/daemon"
)

// linkError prints the sentence the daemon sent, not the raw JSON it arrived in.
func linkError(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, "{"); i >= 0 {
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(msg[i:]), &body) == nil && body.Error != "" {
			return body.Error
		}
	}
	return msg
}

// linkCmd asks the local daemon to enroll this device with a team server and waits for
// the approval. The daemon does the work — it generates the key, signs the enrollment,
// polls, and writes the link beside the store — because it is the store's one writer and
// the process that will use the key. With no daemon there is nothing to ask.
func linkCmd(args []string) {
	server, name := "", ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			name = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--"):
			fmt.Fprintln(os.Stderr, "usage: crossing-guard link <server-url> [--name <device name>]")
			os.Exit(2)
		default:
			server = args[i]
		}
	}
	if server == "" {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard link <server-url> [--name <device name>]")
		os.Exit(2)
	}
	loc, err := daemon.LocateConsole()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no local daemon to ask:", err)
		os.Exit(1)
	}
	var pending struct {
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		Fingerprint     string `json:"fingerprint"`
		ExpiresAt       string `json:"expires_at"`
		Server          string `json:"server"`
		Name            string `json:"name"`
	}
	if err := loc.PostJSON("/api/team/link", map[string]string{"server": server, "name": name}, &pending); err != nil {
		fmt.Fprintln(os.Stderr, "link refused:", linkError(err))
		os.Exit(1)
	}
	fmt.Printf("This device is about to enroll with %s as:\n  name         %s\n  key          %s\n\n", pending.Server, pending.Name, pending.Fingerprint)
	fmt.Printf("Open  %s\nand approve code  %s  (compare the key above with the page)\n\nwaiting for approval", pending.VerificationURL, pending.UserCode)
	// pollInterval is the link verb's own cadence; slack covers one lost poll before
	// the enrollment's expiry (which the daemon also enforces) ends the wait.
	const pollInterval = 2 * time.Second
	const expirySlack = pollInterval + time.Second
	deadline := time.Time{}
	if expires, err := time.Parse(time.RFC3339, pending.ExpiresAt); err == nil {
		deadline = expires.Add(expirySlack)
	}
	for {
		time.Sleep(pollInterval)
		var st struct {
			State        string `json:"state"`
			Problem      string `json:"problem"`
			Organization struct {
				Name string `json:"name"`
			} `json:"organization"`
		}
		if err := loc.GetJSON("/api/team", &st); err != nil {
			fmt.Print("?")
			if !deadline.IsZero() && time.Now().After(deadline) {
				fmt.Printf("\nnot linked: the daemon cannot be asked (last error: %v) and the enrollment code has expired\n", err)
				os.Exit(1)
			}
			continue
		}
		switch {
		case st.State == "linked":
			fmt.Printf("\nlinked to %q — the daemon now sends a device report on its cadence; see Settings → Team\n", st.Organization.Name)
			return
		case st.State == "pending":
			fmt.Print(".")
			if !deadline.IsZero() && time.Now().After(deadline) {
				fmt.Println("\nnot linked: the enrollment code expired before it was approved")
				os.Exit(1)
			}
		default:
			fmt.Printf("\nnot linked: %s\n", st.Problem)
			os.Exit(1)
		}
	}
}

// unlinkCmd asks the daemon to revoke this device's key and forget the link.
func unlinkCmd(_ []string) {
	loc, err := daemon.LocateConsole()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no local daemon to ask:", err)
		os.Exit(1)
	}
	var out struct {
		Note string `json:"note"`
	}
	if err := loc.PostJSON("/api/team/unlink", map[string]string{}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "unlink failed:", linkError(err))
		os.Exit(1)
	}
	fmt.Println(out.Note)
}
