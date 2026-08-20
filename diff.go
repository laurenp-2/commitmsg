package main

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const diffSectionHeader = "\nDiff:\n"

// IsExcluded reports whether a file's contents should be omitted from the
// prompt. Excluded files remain in the file summary so the model still knows
// they were part of the staged change.
func IsExcluded(file FileChange) bool {
	if file.IsBinary {
		return true
	}

	base := path.Base(file.Path)
	switch base {
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum", "Cargo.lock", "Gemfile.lock", "poetry.lock", "composer.lock":
		return true
	}

	return strings.HasSuffix(base, ".min.js") ||
		strings.HasSuffix(base, ".min.css") ||
		strings.HasSuffix(base, ".map")
}

// BuildContext assembles a deterministic, budgeted view of staged changes for
// use in the generation prompt. The file summary is always retained, including
// files whose contents are excluded. The budget is measured in Unicode runes;
// the summary is deliberately allowed to exceed a very small budget because it
// is the one part of the context that must always be present.
func BuildContext(files []FileChange, budget int) string {
	summary := buildFileSummary(files)
	if budget <= runeCount(summary) {
		return summary
	}

	diffBudget := budget - runeCount(summary) - runeCount(diffSectionHeader)
	if diffBudget <= 0 {
		return summary
	}

	candidates := make([]contextFile, 0, len(files))
	for _, file := range files {
		if IsExcluded(file) || file.Diff == "" {
			continue
		}
		candidates = append(candidates, contextFile{
			file:  file,
			giant: isGiantDiff(file, budget),
		})
	}
	if len(candidates) == 0 {
		return summary
	}

	fullBlocks := make([]string, len(candidates))
	fullSize := 0
	for i, candidate := range candidates {
		if candidate.giant {
			fullBlocks[i] = compactFileBlock(candidate.file)
		} else {
			fullBlocks[i] = completeLineBlock(candidate.file.Diff)
		}
		fullSize += runeCount(fullBlocks[i])
	}
	if fullSize <= diffBudget {
		return summary + diffSectionHeader + strings.Join(fullBlocks, "")
	}

	allocations := contextAllocations(candidates, diffBudget)
	blocks := make([]string, 0, len(candidates))
	for i, candidate := range candidates {
		var block string
		if candidate.giant {
			compact := compactFileBlock(candidate.file)
			if runeCount(compact) <= allocations[i] {
				block = compact
			} else {
				block = compactFileBlockWithin(candidate.file, allocations[i])
			}
		} else {
			block = truncatedFileBlock(candidate.file, allocations[i])
		}
		if block != "" {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) == 0 {
		return summary
	}

	return summary + diffSectionHeader + strings.Join(blocks, "")
}

type contextFile struct {
	file  FileChange
	giant bool
}

// contextAllocations reserves exactly the compact representation for giant
// files before distributing the rest across ordinary diffs. Once a giant is
// reduced to its header/stat/marker, giving it more budget cannot add useful
// context; that space belongs to smaller files instead.
func contextAllocations(files []contextFile, budget int) []int {
	allocations := make([]int, len(files))
	if budget <= 0 || len(files) == 0 {
		return allocations
	}

	giantsSize := 0
	normal := make([]contextFile, 0, len(files))
	normalIndexes := make([]int, 0, len(files))
	for i, file := range files {
		if file.giant {
			giantsSize += runeCount(compactFileBlock(file.file))
			continue
		}
		normal = append(normal, file)
		normalIndexes = append(normalIndexes, i)
	}

	// If every compact giant block fits, keep it whole and use the rest where it
	// can still contain real hunks. With an exceptionally tiny budget, fall back
	// to proportional allocation so every file has a fair chance at a marker.
	if giantsSize <= budget && len(normal) > 0 {
		for i, file := range files {
			if file.giant {
				allocations[i] = runeCount(compactFileBlock(file.file))
			}
		}
		normalAllocations := proportionalAllocations(normal, budget-giantsSize)
		for i, allocation := range normalAllocations {
			allocations[normalIndexes[i]] = allocation
		}
		return allocations
	}

	return proportionalAllocations(files, budget)
}

func buildFileSummary(files []FileChange) string {
	var summary strings.Builder
	summary.WriteString("Files changed:\n")
	for _, file := range files {
		fmt.Fprintf(&summary, "- %s +%d/-%d", displayPath(file.Path), file.Added, file.Deleted)
		if file.IsBinary {
			summary.WriteString(" (binary)")
		} else if IsExcluded(file) {
			summary.WriteString(" (excluded)")
		}
		summary.WriteByte('\n')
	}
	return summary.String()
}

func displayPath(filePath string) string {
	// Git paths can legally contain newlines. Keep the summary one file per line
	// without changing normal paths (including paths containing spaces).
	filePath = strings.ReplaceAll(filePath, "\\", "\\\\")
	filePath = strings.ReplaceAll(filePath, "\n", "\\n")
	filePath = strings.ReplaceAll(filePath, "\r", "\\r")
	filePath = strings.ReplaceAll(filePath, "\t", "\\t")
	return filePath
}

func isGiantDiff(file FileChange, totalBudget int) bool {
	return runeCount(file.Diff) > int(math.Floor(0.4*float64(totalBudget)))
}

func proportionalAllocations(files []contextFile, budget int) []int {
	allocations := make([]int, len(files))
	if budget <= 0 || len(files) == 0 {
		return allocations
	}

	weights := make([]float64, len(files))
	totalWeight := 0.0
	for i, file := range files {
		changed := changedLines(file.file)
		if changed > 0 {
			weights[i] = math.Log1p(float64(changed))
			totalWeight += weights[i]
		}
	}
	// A pure rename or mode change has no numstat line count. If every file is
	// like that, allocate evenly instead of dropping the context altogether.
	if totalWeight == 0 {
		for i := range weights {
			weights[i] = 1
		}
		totalWeight = float64(len(weights))
	}

	type remainder struct {
		index    int
		fraction float64
	}
	remainders := make([]remainder, len(files))
	used := 0
	for i, weight := range weights {
		raw := float64(budget) * weight / totalWeight
		base := int(math.Floor(raw))
		allocations[i] = base
		used += base
		remainders[i] = remainder{index: i, fraction: raw - float64(base)}
	}
	sort.SliceStable(remainders, func(i, j int) bool {
		if remainders[i].fraction == remainders[j].fraction {
			return remainders[i].index < remainders[j].index
		}
		return remainders[i].fraction > remainders[j].fraction
	})
	for i := 0; used < budget; i++ {
		allocations[remainders[i%len(remainders)].index]++
		used++
	}
	return allocations
}

func truncatedFileBlock(file FileChange, limit int) string {
	full := completeLineBlock(file.Diff)
	if runeCount(full) <= limit {
		return full
	}

	header, hunks := splitDiff(file.Diff)
	if len(hunks) == 0 {
		return compactFileBlockWithin(file, limit)
	}

	header = completeLineBlock(header)
	marker := func(remaining int) string {
		return fmt.Sprintf("... [%d more hunks truncated]\n", remaining)
	}
	if runeCount(header+marker(len(hunks))) > limit {
		return compactFileBlockWithin(file, limit)
	}

	block := header
	selected := 0
	for i, hunk := range hunks {
		hunk = completeLineBlock(hunk)
		remaining := len(hunks) - i - 1
		candidate := block + hunk + marker(remaining)
		if runeCount(candidate) > limit {
			break
		}
		block += hunk
		selected++
	}
	return block + marker(len(hunks)-selected)
}

func compactFileBlockWithin(file FileChange, limit int) string {
	compact := compactFileBlock(file)
	if runeCount(compact) <= limit {
		return compact
	}

	marker := fmt.Sprintf("[truncated: %d lines changed]\n", changedLines(file))
	if runeCount(marker) <= limit {
		return marker
	}
	return ""
}

func compactFileBlock(file FileChange) string {
	return fmt.Sprintf("%s\nstat: +%d/-%d\n[truncated: %d lines changed]\n",
		diffHeader(file), file.Added, file.Deleted, changedLines(file))
}

func diffHeader(file FileChange) string {
	for _, line := range strings.Split(file.Diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			return strings.TrimSuffix(line, "\r")
		}
	}
	return fmt.Sprintf("diff --git a/%s b/%s", displayPath(file.Path), displayPath(file.Path))
}

func splitDiff(diff string) (string, []string) {
	var header strings.Builder
	var hunks []string
	currentHunk := -1

	for _, line := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(line, "@@") {
			hunks = append(hunks, line)
			currentHunk = len(hunks) - 1
			continue
		}
		if currentHunk < 0 {
			header.WriteString(line)
			continue
		}
		hunks[currentHunk] += line
	}
	return header.String(), hunks
}

func changedLines(file FileChange) int {
	changed := file.Added + file.Deleted
	if changed < 0 {
		return 0
	}
	return changed
}

func completeLineBlock(block string) string {
	if block == "" || strings.HasSuffix(block, "\n") {
		return block
	}
	return block + "\n"
}

func runeCount(value string) int {
	return utf8.RuneCountInString(value)
}
