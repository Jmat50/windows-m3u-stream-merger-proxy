package utils

import (
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

func FileURLToPath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty file URL")
	}

	if !strings.HasPrefix(raw, "file://") {
		return "", fmt.Errorf("not a file URL")
	}

	rawPath := strings.TrimPrefix(raw, "file://")
	if runtime.GOOS == "windows" {
		rawPath = strings.ReplaceAll(rawPath, "/", `\\`)
		if strings.HasPrefix(rawPath, `\\`) && len(rawPath) >= 3 && rawPath[2] == ':' {
			rawPath = strings.TrimPrefix(rawPath, `\\`)
		}
	}

	urlValue, err := url.Parse(raw)
	if err != nil || urlValue.Scheme != "file" {
		return rawPath, nil
	}

	pathValue := urlValue.Path

	if runtime.GOOS == "windows" {
		if urlValue.Host != "" && !strings.EqualFold(urlValue.Host, "localhost") {
			if len(urlValue.Host) == 2 && urlValue.Host[1] == ':' {
				pathValue = urlValue.Host + filepath.FromSlash(pathValue)
			} else {
				pathValue = `\\` + urlValue.Host + filepath.FromSlash(pathValue)
			}
		} else {
			pathValue = filepath.FromSlash(pathValue)
			if strings.HasPrefix(pathValue, `\`) && len(pathValue) >= 3 && pathValue[2] == ':' {
				pathValue = strings.TrimPrefix(pathValue, `\`)
			}
		}
	} else {
		pathValue = filepath.FromSlash(pathValue)
		if urlValue.Host != "" && !strings.EqualFold(urlValue.Host, "localhost") {
			pathValue = `//` + urlValue.Host + pathValue
		}
	}

	if pathValue == "" {
		return rawPath, nil
	}

	return pathValue, nil
}
