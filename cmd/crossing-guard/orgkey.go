package main

// org-key (team rest-of-release plan §4.3, OD-3): the organization's signing key is
// one file the lead keeps wherever they choose, outside the product. These verbs talk
// to no daemon. Nothing in the product stores, reads back, or lists the private key:
// `init` writes it once and refuses a place inside the data directory or a git
// checkout, and `show` prints the public half of a file the lead names.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/daemon"
	"crossing-guard/internal/repofile"
	"crossing-guard/teamwire"
)

const orgKeyUsage = `usage: crossing-guard org-key init --out <path>
       crossing-guard org-key show --key <path>`

// orgKeyPEMType is the PEM block the key file holds: a PKCS #8 private key.
const orgKeyPEMType = "PRIVATE KEY"

// orgKeyCmd is the dispatcher's entry: the data directories are this user's.
func orgKeyCmd(args []string) {
	os.Exit(runOrgKey(args, os.Stdout, os.Stderr, productDataDirs()))
}

// productDataDirs lists the directories that are Crossing Guard data directories for
// this user: the default one and, when a service is installed elsewhere, that one.
func productDataDirs() []string {
	dirs := []string{dataDir()}
	// LocateConsole reads files only; it names the service's data directory even when
	// no token is there yet.
	if loc, _ := daemon.LocateConsole(); loc.DataDir != "" {
		dirs = append(dirs, loc.DataDir)
	}
	return dirs
}

func runOrgKey(args []string, stdout, stderr io.Writer, dataDirs []string) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, orgKeyUsage)
		return 2
	}
	switch {
	case args[0] == "init" && args[1] == "--out":
		return orgKeyInit(args[2], stdout, stderr, dataDirs)
	case args[0] == "show" && args[1] == "--key":
		return orgKeyShow(args[2], stdout, stderr)
	}
	fmt.Fprintln(stderr, orgKeyUsage)
	return 2
}

// orgKeyInit writes a new Ed25519 private key, private to the user, and prints the
// facts an owner pastes into the team console's Register key form.
func orgKeyInit(out string, stdout, stderr io.Writer, dataDirs []string) int {
	path, err := orgKeyPath(out, dataDirs)
	if err != nil {
		fmt.Fprintln(stderr, "org-key init:", err)
		return 1
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(stderr, "org-key init:", err)
		return 1
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		fmt.Fprintln(stderr, "org-key init:", err)
		return 1
	}
	// O_EXCL: an existing file is never overwritten, even one created since the check.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(stderr, "org-key init: %s already exists; a key file is never overwritten — choose another path\n", path)
			return 1
		}
		fmt.Fprintln(stderr, "org-key init:", err)
		return 1
	}
	writeErr := pem.Encode(file, &pem.Block{Type: orgKeyPEMType, Bytes: der})
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = os.Remove(path)
		fmt.Fprintln(stderr, "org-key init:", writeErr)
		return 1
	}
	fmt.Fprintf(stdout, "wrote the organization key to %s (readable by you only)\n", path)
	fmt.Fprintln(stdout, "Crossing Guard keeps no copy. Keep this file safe: anyone who holds it can sign bundles for your organization.")
	printOrgKeyFacts(stdout, public)
	fmt.Fprintln(stdout, "Register it in the team console: Policy → Register key. Paste the key id and the public key, and check that the fingerprint shown there is the one above.")
	return 0
}

// orgKeyPath resolves where init may write: an absolute path whose folder exists,
// that names no existing file, and that is inside neither a Crossing Guard data
// directory nor a git checkout.
func orgKeyPath(out string, dataDirs []string) (string, error) {
	if strings.TrimSpace(out) == "" {
		return "", errors.New("name the file to write with --out")
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	folder, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("the folder %s does not exist", filepath.Dir(absolute))
	}
	path := filepath.Join(folder, filepath.Base(absolute))
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%s already exists; a key file is never overwritten — choose another path", path)
	}
	for _, dir := range dataDirs {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			resolved = filepath.Clean(dir)
		}
		if pathInside(resolved, path) {
			return "", fmt.Errorf("%s is inside the Crossing Guard data directory %s; the organization key is never kept there — choose a place outside it", path, resolved)
		}
	}
	if root, inCheckout := repofile.Locate(folder); inCheckout {
		return "", fmt.Errorf("%s is inside the git checkout %s; a key there can be committed or read by an agent working in it — choose a place outside any checkout", path, root)
	}
	return path, nil
}

func pathInside(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// orgKeyShow prints the public half of a key file again.
func orgKeyShow(path string, stdout, stderr io.Writer) int {
	private, err := readOrgKey(path)
	if err != nil {
		fmt.Fprintln(stderr, "org-key show:", err)
		return 1
	}
	printOrgKeyFacts(stdout, private.Public().(ed25519.PublicKey))
	return 0
}

// printOrgKeyFacts prints the three public facts of a key: the id a bundle's
// signature names, the fingerprint the server computes at registration and a device
// shows at its pin, and the public key text the Register key form takes.
func printOrgKeyFacts(stdout io.Writer, public ed25519.PublicKey) {
	fmt.Fprintf(stdout, "key id:      %s\n", teamwire.OrgKeyID(public))
	fmt.Fprintf(stdout, "fingerprint: %s\n", teamwire.KeyFingerprint(public))
	fmt.Fprintf(stdout, "public key:  %s\n", base64.StdEncoding.EncodeToString(public))
}

// readOrgKey reads an organization key file written by `org-key init`.
func readOrgKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != orgKeyPEMType {
		return nil, fmt.Errorf("%s is not an organization key file written by org-key init", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s is not an organization key file: %w", path, err)
	}
	private, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s holds a key that is not Ed25519", path)
	}
	return private, nil
}
