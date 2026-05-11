package main

// gRPC service wrapper around pkg/store/local. The wrapper is
// intentionally a thin pb⇄[]float32/[]byte translation layer — the
// data structure, sort/merge invariants, and KNN search live in the
// library so the routing module's KNN classifier and other
// in-process callers reuse the same code.

import (
	"github.com/mudler/LocalAI/pkg/grpc/base"
	pb "github.com/mudler/LocalAI/pkg/grpc/proto"
	"github.com/mudler/LocalAI/pkg/store"
	"github.com/mudler/LocalAI/pkg/store/local"
)

type Store struct {
	base.SingleThread
	impl *local.Store
}

func NewStore() *Store {
	return &Store{impl: local.New()}
}

// Load is a no-op — local-store has no on-disk artefact. opts.Model is
// just a namespace identifier; isolation is already handled upstream
// (ModelLoader spawns a fresh local-store process per (backend,
// model) tuple, so each namespace is its own Store{} instance).
func (s *Store) Load(opts *pb.ModelOptions) error {
	_ = opts
	return nil
}

func (s *Store) StoresSet(opts *pb.StoresSetOptions) error {
	return s.impl.Set(store.UnwrapKeys(opts.Keys), store.UnwrapValues(opts.Values))
}

func (s *Store) StoresDelete(opts *pb.StoresDeleteOptions) error {
	return s.impl.Delete(store.UnwrapKeys(opts.Keys))
}

func (s *Store) StoresGet(opts *pb.StoresGetOptions) (pb.StoresGetResult, error) {
	keys, values, err := s.impl.Get(store.UnwrapKeys(opts.Keys))
	if err != nil {
		return pb.StoresGetResult{}, err
	}
	return pb.StoresGetResult{
		Keys:   store.WrapKeys(keys),
		Values: store.WrapValues(values),
	}, nil
}

func (s *Store) StoresFind(opts *pb.StoresFindOptions) (pb.StoresFindResult, error) {
	keys, values, sims, err := s.impl.Find(opts.Key.Floats, int(opts.TopK))
	if err != nil {
		return pb.StoresFindResult{}, err
	}
	return pb.StoresFindResult{
		Keys:         store.WrapKeys(keys),
		Values:       store.WrapValues(values),
		Similarities: sims,
	}, nil
}
