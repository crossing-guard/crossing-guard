package daemon

// Owner-editable config modules (session-view-and-console-preferences plan §B3,
// §B7). A family is one directory of JSON modules: the built-ins are embedded in
// the binary in the same format, and <dataDir>/<family>/<id>.json replaces the
// built-in of its id whole, or adds a module. The console writes installed
// files through one guarded path, so nothing is ever hand-written; a hand edit
// still works, and a file that fails validation is reported and skipped while
// the built-in of that id stays in use.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"crossing-guard/internal/atomicfile"
	"crossing-guard/internal/filelock"
)

// moduleReadLimit bounds one module file in every family; a module is a few
// hundred bytes to a few KiB.
const moduleReadLimit = 64 << 10

var moduleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

var (
	errModuleStale   = errors.New("the module changed since it was read; reload and try again")
	errModuleAbsent  = errors.New("no installed module has that id")
	errModuleInvalid = errors.New("the module is invalid")
	errModuleInUse   = errors.New("the module is selected")
)

// ModuleRejection is a module file that failed and was skipped.
type ModuleRejection struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// ViewProfileRejection keeps the view-profile name the doctor and tests use.
type ViewProfileRejection = ModuleRejection

// moduleFamily is one directory of modules. decode reads one module strictly
// and validates it with whatever context the family needs; builtin says
// whether a module is being read from the embedded set, where some families
// (themes) demand completeness that an installed file may inherit instead.
type moduleFamily[T any] struct {
	subdir   string
	builtins fs.FS
	glob     string
	decode   func(data []byte, builtin bool) (T, error)
	id       func(T) string
}

// resolvedModule is a module in use: where it came from ("builtin" or the
// installed file's path), whether a built-in of its id exists, and the state
// token of its installed file ("" when none is installed).
type resolvedModule[T any] struct {
	Module     T
	Origin     string
	Builtin    bool
	StateToken string
}

func (f moduleFamily[T]) dir(dataDir string) string { return filepath.Join(dataDir, f.subdir) }

func (f moduleFamily[T]) file(dataDir, id string) string {
	return filepath.Join(f.dir(dataDir), id+".json")
}

// loadBuiltins decodes the embedded modules. A failing built-in is a build
// defect, reported like any rejection rather than taking the console down.
func (f moduleFamily[T]) loadBuiltins() (map[string]T, []ModuleRejection) {
	out := map[string]T{}
	rejected := []ModuleRejection{}
	names, _ := fs.Glob(f.builtins, f.glob)
	for _, name := range names {
		data, err := fs.ReadFile(f.builtins, name)
		if err == nil {
			var module T
			if module, err = f.read(data, name, true); err == nil {
				out[f.id(module)] = module
				continue
			}
		}
		rejected = append(rejected, ModuleRejection{Path: "builtin:" + path.Base(name), Error: err.Error()})
	}
	return out, rejected
}

// load resolves every module: the built-ins, then each installed file, which
// replaces the built-in of its id or adds a module. The result is sorted by id.
func (f moduleFamily[T]) load(dataDir string) ([]resolvedModule[T], []ModuleRejection) {
	builtins, rejected := f.loadBuiltins()
	byID := map[string]resolvedModule[T]{}
	for id, module := range builtins {
		byID[id] = resolvedModule[T]{Module: module, Origin: "builtin", Builtin: true}
	}
	installed, _ := filepath.Glob(filepath.Join(f.dir(dataDir), "*.json"))
	for _, file := range installed {
		data, err := readModuleFile(file)
		var module T
		if err == nil {
			module, err = f.read(data, file, false)
		}
		if err != nil {
			rejected = append(rejected, ModuleRejection{Path: file, Error: err.Error()})
			continue
		}
		_, builtin := builtins[f.id(module)]
		byID[f.id(module)] = resolvedModule[T]{Module: module, Origin: file, Builtin: builtin, StateToken: moduleToken(data)}
	}
	out := make([]resolvedModule[T], 0, len(byID))
	for _, module := range byID {
		out = append(out, module)
	}
	sort.Slice(out, func(i, j int) bool { return f.id(out[i].Module) < f.id(out[j].Module) })
	return out, rejected
}

// resolve returns the module of one id, falling back to the built-in of that
// id when the installed file has gone missing or become invalid on disk: a
// selection never points at nothing (plan §B7, per-request degrade).
func (f moduleFamily[T]) resolve(dataDir, id string) (T, bool) {
	modules, _ := f.load(dataDir)
	for _, module := range modules {
		if f.id(module.Module) == id {
			return module.Module, true
		}
	}
	var zero T
	return zero, false
}

// read decodes one module and requires its id to be its file name, so the file
// that replaces a module is always the one named for it.
func (f moduleFamily[T]) read(data []byte, name string, builtin bool) (T, error) {
	module, err := f.decode(data, builtin)
	if err != nil {
		return module, err
	}
	if stem := strings.TrimSuffix(path.Base(filepath.ToSlash(name)), ".json"); stem != f.id(module) {
		var zero T
		return zero, fmt.Errorf("id %q must match the file name %q", f.id(module), stem)
	}
	return module, nil
}

// put installs a module under its id. The body is validated exactly as the
// loader will read it before anything is written, and the caller's token must
// match the installed file ("" when none is installed yet).
func (f moduleFamily[T]) put(dataDir, id, expectedToken string, module T) error {
	if !moduleIDPattern.MatchString(id) {
		return fmt.Errorf("%w: id %q must be lowercase letters, digits and hyphens", errModuleInvalid, id)
	}
	if f.id(module) != id {
		return fmt.Errorf("%w: the body's id %q is not the route's %q", errModuleInvalid, f.id(module), id)
	}
	body, err := json.MarshalIndent(module, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > moduleReadLimit {
		return fmt.Errorf("%w: the module would be larger than %d bytes", errModuleInvalid, moduleReadLimit)
	}
	if _, err := f.read(body, id+".json", false); err != nil {
		return fmt.Errorf("%w: %v", errModuleInvalid, err)
	}
	if err := os.MkdirAll(f.dir(dataDir), 0o700); err != nil {
		return err
	}
	target := f.file(dataDir, id)
	err = filelock.With(target+".lock", 0o600, func() error {
		current, err := installedToken(target)
		if err != nil {
			return err
		}
		if current != expectedToken {
			return errModuleStale
		}
		return atomicfile.Write(target, body, 0o600)
	})
	if err == nil {
		invalidateConsoleConfig() // a selection naming this module re-resolves now
	}
	return err
}

// remove deletes an installed module, so a built-in of that id applies again.
// selectedBy names the selection that would be left pointing at nothing when
// the module has no built-in; a selected module is refused, never orphaned.
func (f moduleFamily[T]) remove(dataDir, id, expectedToken string, selectedBy func(id string) string) error {
	if !moduleIDPattern.MatchString(id) {
		return fmt.Errorf("%w: id %q must be lowercase letters, digits and hyphens", errModuleInvalid, id)
	}
	target := f.file(dataDir, id)
	defer invalidateConsoleConfig()
	return filelock.With(target+".lock", 0o600, func() error {
		raw, err := os.ReadFile(target)
		if errors.Is(err, fs.ErrNotExist) {
			return errModuleAbsent
		}
		if err != nil {
			return err
		}
		if moduleToken(raw) != expectedToken {
			return errModuleStale
		}
		builtins, _ := f.loadBuiltins()
		if _, hasBuiltin := builtins[id]; !hasBuiltin {
			if key := selectedBy(id); key != "" {
				return fmt.Errorf("%w by %s; choose another first", errModuleInUse, key)
			}
		}
		if err := os.Remove(target); err != nil {
			return err
		}
		return atomicfile.SyncDir(f.dir(dataDir))
	})
}

func readModuleFile(name string) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, moduleReadLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > moduleReadLimit {
		return nil, fmt.Errorf("the file is larger than %d bytes", moduleReadLimit)
	}
	return data, nil
}

// decodeModuleStrict decodes one document with no unknown fields and no
// trailing data.
func decodeModuleStrict(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decode: trailing JSON data")
	}
	return nil
}

// moduleToken names one exact file content, the same digest the saved-views
// file uses. An absent file has the empty token.
func moduleToken(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// installedToken is "" only when no file exists. A file that exists but cannot
// be read is an error, never mistaken for "absent" and overwritten.
func installedToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return moduleToken(raw), nil
}

// respondModuleWrite maps a module or selection write error to its status.
func respondModuleWrite(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, errModuleStale), errors.Is(err, errModuleInUse), errors.Is(err, errConsoleConfigStale):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errModuleAbsent):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errModuleInvalid), errors.Is(err, errConsoleConfigInvalid):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	return false
}

// decodeConsoleWriteBody reads one bounded, strictly typed write body.
func decodeConsoleWriteBody(w http.ResponseWriter, r *http.Request, into any) bool {
	config, _ := consoleConfig()
	r.Body = http.MaxBytesReader(w, r.Body, config.WriteBytesMax)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		http.Error(w, "the request could not be read: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
