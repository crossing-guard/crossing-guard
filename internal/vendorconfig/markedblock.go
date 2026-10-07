package vendorconfig

import (
	"fmt"
	"strings"
)

// UpsertMarkedBlock places block (which must begin with begin and end with end)
// in content: replacing the existing begin…end span, else appending it on its
// own lines. A begin marker with no end after it is refused rather than
// guessed at. changed is false when the span already equals block.
func UpsertMarkedBlock(content, begin, end, block string) (out string, changed bool, err error) {
	if !strings.HasPrefix(block, begin) || !strings.HasSuffix(block, end) {
		return content, false, fmt.Errorf("block must start with %q and end with %q", begin, end)
	}
	i := strings.Index(content, begin)
	if i < 0 {
		separator := "\n"
		if content == "" || strings.HasSuffix(content, "\n") {
			separator = ""
		}
		if content != "" {
			separator += "\n"
		}
		return content + separator + block + "\n", true, nil
	}
	j := strings.Index(content[i:], end)
	if j < 0 {
		return content, false, fmt.Errorf("begin marker %q without end marker — fix by hand", begin)
	}
	j += i
	if content[i:j+len(end)] == block {
		return content, false, nil
	}
	return content[:i] + block + content[j+len(end):], true, nil
}

// RemoveMarkedBlock deletes the begin…end span, the newline after it, and the
// blank line UpsertMarkedBlock put before it. An append then a removal gives
// the original bytes back when the original ended in a newline; a file with
// no final newline comes back with one. removed is false when no span is present.
func RemoveMarkedBlock(content, begin, end string) (out string, removed bool, err error) {
	i := strings.Index(content, begin)
	if i < 0 {
		return content, false, nil
	}
	j := strings.Index(content[i:], end)
	if j < 0 {
		return content, false, fmt.Errorf("begin marker %q without end marker — fix by hand", begin)
	}
	j += i + len(end)
	if j < len(content) && content[j] == '\n' {
		j++
	}
	if strings.HasSuffix(content[:i], "\n\n") {
		i--
	}
	return content[:i] + content[j:], true, nil
}

// MarkedBlock returns the begin…end span, or "" when none is present.
func MarkedBlock(content, begin, end string) string {
	i := strings.Index(content, begin)
	if i < 0 {
		return ""
	}
	j := strings.Index(content[i:], end)
	if j < 0 {
		return ""
	}
	return content[i : i+j+len(end)]
}
