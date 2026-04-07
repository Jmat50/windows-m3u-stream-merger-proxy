package loadbalancer

import (
	"context"
	"reflect"
	"testing"
)

func TestWithExcludedIndexes(t *testing.T) {
	ctx := context.Background()
	ctxWithExcluded := WithExcludedIndexes(ctx, []string{" 2 ", "", "1", "2"})

	got := excludedIndexesFromContext(ctxWithExcluded)
	want := map[string]struct{}{
		"1": {},
		"2": {},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("excludedIndexesFromContext() = %#v, want %#v", got, want)
	}
}

func TestWithExcludedIndexesEmptyInput(t *testing.T) {
	ctx := context.Background()

	ctxNoExcluded := WithExcludedIndexes(ctx, []string{"", "   "})
	if got := excludedIndexesFromContext(ctxNoExcluded); got != nil {
		t.Fatalf("excludedIndexesFromContext() = %#v, want nil", got)
	}
}
