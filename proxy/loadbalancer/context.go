package loadbalancer

import (
	"context"
	"strings"
)

type excludedIndexesContextKey struct{}

// WithExcludedIndexes sets preferred source indexes to skip for this
// load-balancer call only (for example, after a source stalls mid-stream).
func WithExcludedIndexes(ctx context.Context, indexes []string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	if len(indexes) == 0 {
		return ctx
	}

	normalized := make(map[string]struct{}, len(indexes))
	for _, index := range indexes {
		trimmed := strings.TrimSpace(index)
		if trimmed == "" {
			continue
		}
		normalized[trimmed] = struct{}{}
	}

	if len(normalized) == 0 {
		return ctx
	}

	return context.WithValue(ctx, excludedIndexesContextKey{}, normalized)
}

func excludedIndexesFromContext(ctx context.Context) map[string]struct{} {
	if ctx == nil {
		return nil
	}

	value := ctx.Value(excludedIndexesContextKey{})
	indexes, ok := value.(map[string]struct{})
	if !ok || len(indexes) == 0 {
		return nil
	}

	return indexes
}
