package workload

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CacheThenLive returns a reader that reads from cache and falls back to
// live on NotFound. A cached object can only lag, so a hit is cheap. A miss
// may be an object too new for this replica's informers or one the cache's
// filter leaves out, and the API server answers for both.
func CacheThenLive(cache, live client.Reader) client.Reader {
	return cacheThenLive{cache: cache, live: live}
}

type cacheThenLive struct {
	cache client.Reader
	live  client.Reader
}

// Get reads key from the cache, then from the API server if the cache has
// no such object.
func (r cacheThenLive) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.cache.Get(ctx, key, obj, opts...)
	if apierrors.IsNotFound(err) {
		return r.live.Get(ctx, key, obj, opts...)
	}
	return err
}

// List reads from the cache. A list has no miss to fall back on.
func (r cacheThenLive) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return r.cache.List(ctx, list, opts...)
}
