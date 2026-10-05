package daemon

// A line diff for a shared agent's text (team rest-of-release plan §4.1 decision 3:
// "the text diff shown before Adopt"; §4.3: `bundle build` prints each profile's text
// diff). It anchors on lines that occur exactly once on both sides and recurses
// between the anchors, so its cost is bounded by the input and needs no size limit.

import (
	"sort"
	"strings"
)

// Line operations of a text diff. They are data a surface draws.
const (
	textDiffKeep   = "keep"
	textDiffAdd    = "add"
	textDiffRemove = "remove"
)

// textDiffLine is one line of a diff with what happened to it.
type textDiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// textDiff returns the line diff from before to after.
func textDiff(before, after string) []textDiffLine {
	out := []textDiffLine{}
	diffLines(splitDiffLines(before), splitDiffLines(after), &out)
	return out
}

func splitDiffLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func diffLines(before, after []string, out *[]textDiffLine) {
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		*out = append(*out, textDiffLine{Op: textDiffKeep, Text: before[prefix]})
		prefix++
	}
	before, after = before[prefix:], after[prefix:]
	suffix := 0
	for suffix < len(before) && suffix < len(after) && before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	tail := before[len(before)-suffix:]
	before, after = before[:len(before)-suffix], after[:len(after)-suffix]

	anchors := uniqueAnchors(before, after)
	if len(anchors) == 0 {
		for _, line := range before {
			*out = append(*out, textDiffLine{Op: textDiffRemove, Text: line})
		}
		for _, line := range after {
			*out = append(*out, textDiffLine{Op: textDiffAdd, Text: line})
		}
	} else {
		fromBefore, fromAfter := 0, 0
		for _, anchor := range anchors {
			diffLines(before[fromBefore:anchor[0]], after[fromAfter:anchor[1]], out)
			*out = append(*out, textDiffLine{Op: textDiffKeep, Text: before[anchor[0]]})
			fromBefore, fromAfter = anchor[0]+1, anchor[1]+1
		}
		diffLines(before[fromBefore:], after[fromAfter:], out)
	}
	for _, line := range tail {
		*out = append(*out, textDiffLine{Op: textDiffKeep, Text: line})
	}
}

// uniqueAnchors pairs the lines that occur exactly once in each side and keeps the
// longest run of pairs that is in order on both sides.
func uniqueAnchors(before, after []string) [][2]int {
	type seen struct{ count, index int }
	inBefore, inAfter := map[string]*seen{}, map[string]*seen{}
	count := func(lines []string, into map[string]*seen) {
		for index, line := range lines {
			if entry := into[line]; entry != nil {
				entry.count++
			} else {
				into[line] = &seen{count: 1, index: index}
			}
		}
	}
	count(before, inBefore)
	count(after, inAfter)
	pairs := [][2]int{}
	for line, entry := range inBefore {
		if other := inAfter[line]; entry.count == 1 && other != nil && other.count == 1 {
			pairs = append(pairs, [2]int{entry.index, other.index})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	return longestIncreasing(pairs)
}

// longestIncreasing returns the longest subsequence of pairs (sorted by their first
// index) whose second index increases.
func longestIncreasing(pairs [][2]int) [][2]int {
	tails := []int{} // tails[k] = index into pairs of the smallest tail of a run of length k+1
	previous := make([]int, len(pairs))
	for index, pair := range pairs {
		at := sort.Search(len(tails), func(k int) bool { return pairs[tails[k]][1] >= pair[1] })
		previous[index] = -1
		if at > 0 {
			previous[index] = tails[at-1]
		}
		if at == len(tails) {
			tails = append(tails, index)
		} else {
			tails[at] = index
		}
	}
	if len(tails) == 0 {
		return nil
	}
	run := make([][2]int, len(tails))
	for index, at := tails[len(tails)-1], len(tails)-1; at >= 0; index, at = previous[index], at-1 {
		run[at] = pairs[index]
	}
	return run
}

// textChanged reports whether a diff holds any added or removed line.
func textChanged(lines []textDiffLine) bool {
	for _, line := range lines {
		if line.Op != textDiffKeep {
			return true
		}
	}
	return false
}
