package daemon

import "path/filepath"

// A place (managed binding) fires for work in its folder, and a folder has
// more than one spelling: macOS's /tmp is /private/tmp, the session rail folds
// that prefix away while vendors record the physical path, a symlink or a
// firmlink names another folder's contents, and a case-insensitive volume
// answers any case. Comparing cleaned strings made a place saved from the rail
// silently never fire.

// projectRootKey is the identity of the folder a path names: its physical path
// as the operating system reports it for the open folder. It is a comparison
// value, never a path to open, store or show. A folder that cannot be opened
// (gone, not a directory, refused by the OS) falls back to its symlink-resolved
// spelling, then to its cleaned spelling; case is never folded here, so two
// distinct folders never share a key. Empty and relative paths have no key.
func projectRootKey(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return ""
	}
	if physical, err := physicalDirectory(clean); err == nil {
		return physical
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	return clean
}

// folderScope decides, for one working directory, which place roots name its
// folder. The rule is the one the browser mirror applies to the published keys
// (agentWatchesSession): a non-empty root matches when its cleaned spelling
// equals the directory's, or when both keys are non-empty and equal. Equal
// spellings never touch the file system; the directory is resolved at most
// once, so a routing pass shares one scope across all of its bindings.
type folderScope struct {
	dir      string // cleaned absolute directory; "" matches nothing
	key      string
	resolved bool
}

func newFolderScope(dir string) *folderScope {
	scope := &folderScope{}
	if dir != "" {
		if clean := filepath.Clean(dir); filepath.IsAbs(clean) {
			scope.dir = clean
		}
	}
	return scope
}

// Key is the directory's projectRootKey, resolved on first use.
func (s *folderScope) Key() string {
	if !s.resolved {
		s.key, s.resolved = projectRootKey(s.dir), true
	}
	return s.key
}

func (s *folderScope) matchesRoot(root string) bool {
	if root == "" || s.dir == "" {
		return false
	}
	if filepath.Clean(root) == s.dir {
		return true
	}
	key := s.Key()
	return key != "" && projectRootKey(root) == key
}
