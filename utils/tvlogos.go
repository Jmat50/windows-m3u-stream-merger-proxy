package utils

import (
	"os"
	"path/filepath"
	"strings"
)

var tvLogosRootOverride string

func SetTVLogosRootOverrideForTests(rootDir string) {
	tvLogosRootOverride = rootDir
}

func ResetTVLogosRootOverrideForTests() {
	tvLogosRootOverride = ""
}

func ResolveTVLogosRootDir() string {
	if strings.TrimSpace(tvLogosRootOverride) != "" {
		return tvLogosRootOverride
	}

	if dir := strings.TrimSpace(os.Getenv("TV_LOGOS_DIR")); dir != "" {
		if stat, err := os.Stat(dir); err == nil && stat.IsDir() {
			return dir
		}
	}

	candidates := make([]string, 0, 6)
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "tvlogos"),
			filepath.Join(exeDir, "..", "tvlogos"),
		)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "tvlogos"))
	}
	candidates = append(candidates, "tvlogos", filepath.Join("..", "tvlogos"))

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}

		absCandidate, err := filepath.Abs(candidate)
		if err != nil {
			absCandidate = candidate
		}
		if _, ok := seen[absCandidate]; ok {
			continue
		}
		seen[absCandidate] = struct{}{}

		stat, err := os.Stat(absCandidate)
		if err != nil || !stat.IsDir() {
			continue
		}
		return absCandidate
	}

	return ""
}
