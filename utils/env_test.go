package utils

import (
	"os"
	"testing"
)

func TestGetM3UIndexes_SortsByNumericIndex(t *testing.T) {
	ResetCaches()
	defer ResetCaches()

	_ = os.Unsetenv("M3U_URL_1")
	_ = os.Unsetenv("M3U_URL_2")
	_ = os.Unsetenv("M3U_URL_3")
	_ = os.Unsetenv("M3U_URL_10")
	defer func() {
		_ = os.Unsetenv("M3U_URL_1")
		_ = os.Unsetenv("M3U_URL_2")
		_ = os.Unsetenv("M3U_URL_3")
		_ = os.Unsetenv("M3U_URL_10")
	}()

	_ = os.Setenv("M3U_URL_10", "http://example.com/10.m3u")
	_ = os.Setenv("M3U_URL_2", "http://example.com/2.m3u")
	_ = os.Setenv("M3U_URL_1", "http://example.com/1.m3u")

	got := GetM3UIndexes()
	want := []string{"1", "2", "10"}

	if len(got) != len(want) {
		t.Fatalf("GetM3UIndexes() length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GetM3UIndexes()[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

func TestGetFilters_SortsByNumericSuffix(t *testing.T) {
	ResetCaches()
	defer ResetCaches()

	_ = os.Unsetenv("INCLUDE_TITLE_1")
	_ = os.Unsetenv("INCLUDE_TITLE_2")
	_ = os.Unsetenv("INCLUDE_TITLE_10")
	defer func() {
		_ = os.Unsetenv("INCLUDE_TITLE_1")
		_ = os.Unsetenv("INCLUDE_TITLE_2")
		_ = os.Unsetenv("INCLUDE_TITLE_10")
	}()

	_ = os.Setenv("INCLUDE_TITLE_10", "ten")
	_ = os.Setenv("INCLUDE_TITLE_2", "two")
	_ = os.Setenv("INCLUDE_TITLE_1", "one")

	got := GetFilters("INCLUDE_TITLE")
	want := []string{"one", "two", "ten"}

	if len(got) != len(want) {
		t.Fatalf("GetFilters() length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GetFilters()[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

