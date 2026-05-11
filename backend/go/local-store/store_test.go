package main

// Wrapper tests for the gRPC service. Algorithm coverage lives in
// pkg/store/local; this file pins the pb⇄[]float32/[]byte
// translation contract so a regression in the wire shape can't break
// gRPC consumers without a test failure.

import (
	"testing"

	pb "github.com/mudler/LocalAI/pkg/grpc/proto"
)

func TestWrapper_SetGetFindRoundTrip(t *testing.T) {
	s := NewStore()

	if err := s.StoresSet(&pb.StoresSetOptions{
		Keys:   []*pb.StoresKey{{Floats: []float32{1, 0, 0}}, {Floats: []float32{0, 1, 0}}},
		Values: []*pb.StoresValue{{Bytes: []byte("x")}, {Bytes: []byte("y")}},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := s.StoresGet(&pb.StoresGetOptions{
		Keys: []*pb.StoresKey{{Floats: []float32{1, 0, 0}}},
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Keys) != 1 || string(got.Values[0].Bytes) != "x" {
		t.Errorf("Get round-trip mismatch: keys=%d values=%q", len(got.Keys), got.Values[0].Bytes)
	}

	res, err := s.StoresFind(&pb.StoresFindOptions{
		Key:  &pb.StoresKey{Floats: []float32{1, 0, 0}},
		TopK: 1,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Keys) != 1 || string(res.Values[0].Bytes) != "x" {
		t.Errorf("Find round-trip mismatch: keys=%d values=%q", len(res.Keys), res.Values[0].Bytes)
	}
	if len(res.Similarities) != 1 {
		t.Errorf("Find returned %d similarities, want 1", len(res.Similarities))
	}
}

func TestWrapper_DeleteRemovesEntry(t *testing.T) {
	s := NewStore()
	_ = s.StoresSet(&pb.StoresSetOptions{
		Keys:   []*pb.StoresKey{{Floats: []float32{1, 0, 0}}},
		Values: []*pb.StoresValue{{Bytes: []byte("x")}},
	})
	if err := s.StoresDelete(&pb.StoresDeleteOptions{
		Keys: []*pb.StoresKey{{Floats: []float32{1, 0, 0}}},
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ := s.StoresGet(&pb.StoresGetOptions{
		Keys: []*pb.StoresKey{{Floats: []float32{1, 0, 0}}},
	})
	if len(got.Keys) != 0 {
		t.Errorf("expected entry deleted, got %d keys", len(got.Keys))
	}
}

func TestWrapper_LoadIsNoOp(t *testing.T) {
	s := NewStore()
	if err := s.Load(&pb.ModelOptions{Model: "any-namespace"}); err != nil {
		t.Errorf("Load should be no-op, got %v", err)
	}
}
