package daemon

// The codemap surface: GET /api/codemap/descriptor — the L3 cartography answer
// for one file (console-and-info-panel §7 L3).
//
// The daemon host injects one immutable analyzer assembly and loads convention configs
// that map project conventions onto roles. codemap core never names a reference
// language. Adding/replacing a selected process module changes installed bytes and
// selection, not this request path.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"crossing-guard/codemap"
	"crossing-guard/internal/analyzerhost"
)

var (
	describersMu sync.Mutex
	describers   = map[string]*codemap.Describer{}
)

// conventionConfigFor finds the convention config for a project. A project may
// ship its own at .crossing-guard/conventions.json; otherwise we fall back to
// the default that ships with the binary. A missing config is NOT fatal — roles
// then report "unknown, no config loaded", which is the honest answer.
func conventionConfigFor(root string) (*codemap.Config, string, error) {
	candidates := []string{
		filepath.Join(root, ".crossing-guard", "conventions.json"),
		filepath.Join(root, "codemap", "configs", "go-conventions.json"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			cfg, err := codemap.LoadConfig(c)
			if err != nil {
				return nil, c, err
			}
			return cfg, c, nil
		}
	}
	return nil, "", nil
}

func describerFor(root string) (*codemap.Describer, string, error) {
	abs, err := projectRoot(root)
	if err != nil {
		return nil, "", err
	}
	describersMu.Lock()
	defer describersMu.Unlock()
	assembly := currentAnalyzerAssembly()
	key := abs + "\x00" + assembly.Digest()
	if d, ok := describers[key]; ok {
		return d, describerConfigPath[abs], nil
	}
	cfg, cfgPath, err := conventionConfigFor(abs)
	if err != nil {
		return nil, cfgPath, err
	}
	d := codemap.NewDescriberWithAssembly(abs, cfg, assembly)
	describers[key] = d
	describerConfigPath[abs] = cfgPath
	return d, cfgPath, nil
}

func currentAnalyzerBundle() string {
	return currentAnalyzerAssembly().Digest()
}

func currentAnalyzerAssembly() *codemap.AnalyzerAssembly {
	if governor != nil && governor.analyzerAssembly != nil {
		return governor.analyzerAssembly
	}
	assembly, err := analyzerhost.Compatibility()
	if err != nil {
		panic(err)
	}
	return assembly
}

var describerConfigPath = map[string]string{}

// DescriptorResponse wraps the descriptor with what the HOST knows: which config
// was applied and which languages are bound. Without those, an empty role reads
// like a property of the code rather than a gap in configuration.
type DescriptorResponse struct {
	Descriptor *codemap.UnitDescriptor `json:"descriptor"`
	Config     string                  `json:"config,omitempty"`
	Languages  []string                `json:"languages"`
	Note       string                  `json:"note,omitempty"`
}

func handleCodemapDescriptor(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("cwd")
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	d, cfgPath, err := describerFor(root)
	if err != nil {
		http.Error(w, err.Error(), refRequestStatus(err))
		return
	}
	desc, err := d.Describe(rel)
	if err != nil {
		// "No adapter claims this file" is a normal answer, not a failure: say it
		// plainly with what IS bound, so the reader can tell a missing adapter
		// from a broken one.
		var none *codemap.ErrNoAnalyzer
		if errors.As(err, &none) {
			writeJSON(w, DescriptorResponse{
				Languages: currentAnalyzerAssembly().Languages(), Config: cfgPath,
				Note: none.Error(),
			})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := DescriptorResponse{Descriptor: desc, Config: cfgPath, Languages: currentAnalyzerAssembly().Languages()}
	if cfgPath == "" {
		resp.Note = "no convention config found for this project, so no role can be assigned"
	}
	writeJSON(w, resp)
}

// Doc-linked intent (§7 L4) is DELIBERATELY NOT IMPLEMENTED HERE, and this
// comment is the record of why, so the next person does not rebuild it.
//
// The first attempt took the first backlink — any doc line containing the
// filename — cut it at the line break, and rendered it under "INTENT ·
// DOC-LINKED" as the answer to "what is this file supposed to do". What that
// actually produced, live:
//
//	describe.go  → a sentence from an AUDIT criticising describe.go
//	role.go      → a hypothetical detector output from a doc about UNBUILT work
//	govern.go    → a fragment of the design doc's own bullet list, cut mid-token
//
// A mention is not a statement of intent. The field could not tell a spec from
// a critique from a to-do, it changed between page loads because the backlink
// list came out of a map, and it got worse every time anyone wrote a document.
// Tagged `inferred`, it read as a judgement when nothing had judged anything.
//
// Intent returns when it can cite a DECLARATION rather than a mention — a
// D-item whose Subjects name this file (that mechanism already exists and is
// already computed), or the `// D5:` code-comment convention design §14 raises.
// Until then the panel renders no intent block, which is true.
