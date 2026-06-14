package sourceproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"windows-m3u-stream-merger-proxy/utils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bundledTVLogosDir(t *testing.T) string {
	t.Helper()

	candidates := []string{
		filepath.Join("..", "tvlogos"),
		"tvlogos",
	}
	for _, candidate := range candidates {
		absPath, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		unitedStatesDir := filepath.Join(absPath, "countries", "united-states")
		if stat, statErr := os.Stat(unitedStatesDir); statErr == nil && stat.IsDir() {
			return absPath
		}
	}

	t.Skip("bundled tvlogos directory not found")
	return ""
}

func useBundledTVLogos(t *testing.T) {
	t.Helper()

	rootDir := bundledTVLogosDir(t)
	utils.SetTVLogosRootOverrideForTests(rootDir)
	autoChannelIconCache.resetForTests()

	t.Cleanup(func() {
		utils.ResetTVLogosRootOverrideForTests()
		autoChannelIconCache.resetForTests()
	})
}

func TestLoadLocalTVLogoCandidates_BundledDataset(t *testing.T) {
	rootDir := bundledTVLogosDir(t)

	candidates, err := loadLocalTVLogoCandidates(rootDir)
	require.NoError(t, err)
	assert.Greater(t, len(candidates), 1000)

	keys := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		keys[candidate.key] = struct{}{}
	}
	assert.Contains(t, keys, "discovery-channel")
	assert.Contains(t, keys, "fox")
}

func TestParseLine_AutoAssignsBundledLogos(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	useBundledTVLogos(t)
	t.Setenv("AUTO_RETRIEVE_CHANNEL_ICONS", "true")

	tests := []struct {
		name         string
		line         string
		expectedFile string
	}{
		{
			name:         "discovery channel",
			line:         `#EXTINF:-1 tvg-id="discovery.us" tvg-name="Discovery Channel",Discovery Channel`,
			expectedFile: "countries/united-states/discovery-channel-us.png",
		},
		{
			name:         "fox east suffix",
			line:         `#EXTINF:-1 tvg-id="fox.us" tvg-name="FOX US East",FOX US East`,
			expectedFile: "countries/united-states/fox-us.png",
		},
		{
			name:         "cnn hd suffix",
			line:         `#EXTINF:-1 tvg-id="cnn.us" tvg-name="CNN US HD",CNN US HD`,
			expectedFile: "countries/united-states/cnn-us.png",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stream := parseLine(
				tc.line,
				&LineDetails{Content: "http://example.com/stream", LineNum: 1},
				"M3U_Test",
			)
			require.NotNil(t, stream)
			assert.True(t, stream.AutoLogoURL)
			assert.Equal(t, expectedAutoChannelIconURL(tc.expectedFile), stream.LogoURL)
		})
	}
}

func TestParseLine_DoesNotOverrideSourceLogoWithBundledLogos(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	useBundledTVLogos(t)
	t.Setenv("AUTO_RETRIEVE_CHANNEL_ICONS", "true")

	sourceLogo := utils.TvgLogoParser("http://logo/fox-source.png")
	stream := parseLine(
		`#EXTINF:-1 tvg-id="fox.us" tvg-name="FOX US East" tvg-logo="http://logo/fox-source.png",FOX US East`,
		&LineDetails{Content: "http://example.com/fox", LineNum: 1},
		"M3U_Test",
	)
	require.NotNil(t, stream)
	assert.False(t, stream.AutoLogoURL)
	assert.Equal(t, sourceLogo, stream.LogoURL)
}

func TestBuildChannelIconLookupKeys_TrimsRegionalSuffixes(t *testing.T) {
	keys := buildChannelIconLookupKeys("NBC West HD USA", "")
	assert.Contains(t, keys, "nbc-west-hd-usa")
	assert.Contains(t, keys, "nbc")
	assert.True(t, strings.HasSuffix(keys[0], "usa") || strings.HasSuffix(keys[0], "hd"))
}
