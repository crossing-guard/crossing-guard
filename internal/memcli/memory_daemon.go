package memcli

// memory_daemon.go — the memory READ verbs (search, get, list, index) read
// through the daemon, the store's only reader (memory-reads-through-daemon
// plan §4.2). They open no SQLite handle: a helper or hook that shells out to
// them never touches index.sqlite, and search has one implementation (the
// daemon's). Write verbs and doctor still use memory_store.go's direct handle
// until their own plan moves them.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/memory"
	"crossing-guard/store"
)

// Daemon is the located daemon as the read verbs use it: an authenticated
// JSON reader and the address it listens on.
type Daemon struct {
	Reader interface {
		GetJSON(path string, out any) error
	}
	Addr string
}

// locateDaemon is set by Main from the caller (cmd/crossing-guard passes the
// same located, tokened client the recall server uses). Located per verb, so
// nothing is cached across a daemon restart.
var locateDaemon func() (Daemon, error)

// memoryConfig is the part of GET /api/memory/config the read verbs use: the
// index the daemon actually reads, and the search cap.
type memoryConfig struct {
	Search struct {
		MemorySearchLimitMax int `json:"memory_search_limit_max"`
	} `json:"search"`
	Store struct {
		IndexPath string `json:"index_path"`
	} `json:"store"`
}

// memoryDaemon locates the daemon and proves it reads the store this CLI
// writes. The daemon names its own index (it alone knows whether --data or
// $CG_INDEX chose it); the CLI's is what its write verbs open (CG_INDEX /
// CPMEM_INDEX, else ~/.crossing-guard). A mismatch refuses, naming both — a
// read never switches stores silently.
func memoryDaemon() (Daemon, memoryConfig, error) {
	var config memoryConfig
	if locateDaemon == nil {
		return Daemon{}, config, errors.New("memory reads go through the Crossing Guard daemon, and none is configured for this command")
	}
	d, err := locateDaemon()
	if err != nil {
		return Daemon{}, config, fmt.Errorf("memory reads go through the Crossing Guard daemon, which could not be located: %v (run: crossing-guard doctor)", err)
	}
	if err := daemonGet(d, "/api/memory/config", &config); err != nil {
		return Daemon{}, config, err
	}
	daemonStore, cliStore := config.Store.IndexPath, indexDB()
	if daemonStore == "" {
		return Daemon{}, config, fmt.Errorf("the daemon at %s does not say which store it reads (an older daemon?); "+
			"memory reads refuse rather than guess (run: crossing-guard doctor)", d.Addr)
	}
	if resolvedPath(daemonStore) != resolvedPath(cliStore) {
		return Daemon{}, config, fmt.Errorf("the daemon at %s reads %s, but this command's store is %s "+
			"(CG_INDEX / CPMEM_INDEX, or a daemon started with a different --data); "+
			"memory reads refuse to read a different store than memory writes use", d.Addr, daemonStore, cliStore)
	}
	return d, config, nil
}

// resolvedPath compares store files by location, not spelling: symlinks in the
// path (e.g. /tmp → /private/tmp) resolve, and a file that does not exist yet
// resolves through its directory.
func resolvedPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// daemonGet reads one route, turning a transport failure into the one message
// that says what to do; an answer from the daemon keeps the daemon's reason.
func daemonGet(d Daemon, path string, out any) error {
	err := d.Reader.GetJSON(path, out)
	var netErr net.Error
	var opErr *net.OpError
	if err != nil && (errors.As(err, &opErr) || errors.As(err, &netErr)) {
		return fmt.Errorf("memory reads go through the Crossing Guard daemon at %s, which did not answer: %v (run: crossing-guard doctor)", d.Addr, err)
	}
	if err != nil && (strings.Contains(err.Error(), ": 401 ") || strings.Contains(err.Error(), ": 403 ")) {
		return fmt.Errorf("%v (run: crossing-guard doctor)", err)
	}
	return err
}

// memorySearchResponse is GET /api/memory/search's answer (the route owns it).
type memorySearchResponse struct {
	Hits []struct {
		ID      string   `json:"id"`
		Title   string   `json:"title"`
		Pending bool     `json:"pending"`
		Score   int      `json:"score"`
		Why     []string `json:"why"`
	} `json:"hits"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// LimitMax is the daemon's search cap (from /api/memory/config), for the
	// truncation hint; not part of the route's answer.
	LimitMax int `json:"-"`
}

func daemonSearch(query, category, repository, tag string, limit int) (memorySearchResponse, rawJSON, error) {
	var resp memorySearchResponse
	d, config, err := memoryDaemon()
	if err != nil {
		return resp, nil, err
	}
	resp.LimitMax = config.Search.MemorySearchLimitMax
	v := url.Values{"via": {"cli"}}
	for key, value := range map[string]string{"q": query, "category": category, "repository": repository, "tag": tag} {
		if value != "" {
			v.Set(key, value)
		}
	}
	if limit > 0 {
		v.Set("limit", fmt.Sprint(limit))
	}
	var raw rawJSON
	if err := daemonGet(d, "/api/memory/search?"+v.Encode(), &raw); err != nil {
		return resp, nil, err
	}
	return resp, raw, raw.decode(&resp)
}

// memoryRecordView is the record map GET /api/memory/record and
// GET /api/memory/records share (the daemon's memoryRecordMap).
type memoryRecordView struct {
	ID           string               `json:"id"`
	Title        string               `json:"title"`
	Category     string               `json:"category"`
	ScopeType    string               `json:"scope_type"`
	Repository   string               `json:"repository"`
	Tags         []string             `json:"tags"`
	Aliases      []string             `json:"aliases"`
	Source       string               `json:"source"`
	Origin       string               `json:"origin"`
	SupersededBy string               `json:"superseded_by"`
	VerifiedAt   string               `json:"verified_at"`
	VerifiedBy   string               `json:"verified_by"`
	Created      string               `json:"created"`
	Updated      string               `json:"updated"`
	Body         string               `json:"body"`
	Revision     int64                `json:"revision"`
	Status       string               `json:"status"`
	Sources      []store.MemorySource `json:"sources"`
	// Scope as recall needs it (team item 5 decision 12).
	ScopeID            string `json:"scope_id"`
	RepositoryIdentity string `json:"repository_identity"`
	Collision          string `json:"collision"`
}

func daemonRecord(id string) (memoryRecordView, rawJSON, error) {
	var rec memoryRecordView
	d, _, err := memoryDaemon()
	if err != nil {
		return rec, nil, err
	}
	var raw rawJSON
	if err := daemonGet(d, "/api/memory/record?"+url.Values{"id": {id}, "via": {"cli"}}.Encode(), &raw); err != nil {
		return rec, nil, err
	}
	return rec, raw, raw.decode(&rec)
}

func daemonRecords(status string) ([]memoryRecordView, rawJSON, error) {
	recs, raw, _, err := daemonRecordsScoped(status, "")
	return recs, raw, err
}

// daemonRecordsScoped reads one status's records and, when cwd is given, the recall
// scope the daemon resolved for it (its repository label and remote-derived id).
func daemonRecordsScoped(status, cwd string) ([]memoryRecordView, rawJSON, memory.RecallScope, error) {
	var resp struct {
		Records     rawJSON            `json:"records"`
		RecallScope memory.RecallScope `json:"recall_scope"`
	}
	d, _, err := memoryDaemon()
	if err != nil {
		return nil, nil, memory.RecallScope{}, err
	}
	query := url.Values{"status": {status}}
	if cwd != "" {
		query.Set("cwd", cwd)
	}
	if err := daemonGet(d, "/api/memory/records?"+query.Encode(), &resp); err != nil {
		return nil, nil, memory.RecallScope{}, err
	}
	var recs []memoryRecordView
	if err := resp.Records.decode(&recs); err != nil {
		return nil, nil, memory.RecallScope{}, err
	}
	return recs, resp.Records, resp.RecallScope, nil
}

// memIndexBlock builds the injection block from the daemon's active records.
// The nonce and its recall-log line stay here (doctor proves an injection by
// that nonce); a daemon that cannot answer is said so, never injected as an
// empty index (the injection health contract).
//
// The hook sends its cwd and the daemon answers the session's recall scope — its
// repository label and, when the checkout resolves to one remote, that repository's id
// (team item 5 decision 12). This process forks no git: with no --project, the scope is
// the daemon's; a daemon that could not resolve the folder leaves the folder's own name.
func memIndexBlock(maxBytes int, project string) string {
	cwd, _ := os.Getwd()
	recs, _, scope, err := daemonRecordsScoped("active", cwd)
	if err != nil {
		return "(crossing-guard memory store unavailable: " + err.Error() + ")\n"
	}
	if project != "" {
		scope.Label = project // an explicit --project is the caller's label filter
	} else if scope.Label == "" && cwd != "" {
		scope.Label = filepath.Base(cwd)
	}
	var file []memory.Record
	for _, r := range recs {
		rec := memory.RecordFromStore(r.ID, r.Status, r.ScopeType, r.Repository,
			r.Title, r.Category, r.Body, r.Tags, r.Aliases, r.Source, r.Origin,
			r.SupersededBy, r.VerifiedAt, r.VerifiedBy, r.Created, r.Updated)
		rec.RepositoryIdentity, rec.Shadowed = r.RepositoryIdentity, r.Collision == "shadowed"
		file = append(file, rec)
	}
	return memory.BuildIndexForScope(memory.DefaultDir(), maxBytes, scope, file)
}

// rawJSON keeps the daemon's answer byte-for-byte for --json output (D-1: the
// CLI prints the route's shape) while the human output decodes it.
type rawJSON []byte

func (r *rawJSON) UnmarshalJSON(b []byte) error {
	*r = append((*r)[:0], b...)
	return nil
}

func (r rawJSON) decode(out any) error { return json.Unmarshal(r, out) }

// print writes the daemon's answer indented, as printJSON does for local values.
func (r rawJSON) print() {
	var out bytes.Buffer
	if err := json.Indent(&out, r, "", "  "); err != nil {
		fmt.Println(string(r)) // not indentable: print the daemon's bytes as sent
		return
	}
	fmt.Println(out.String())
}
