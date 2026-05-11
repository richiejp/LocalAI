package local

import (
	"math"
	"math/rand/v2"
	"testing"
)

// Regression suite for the in-process Store. The gRPC backend at
// backend/go/local-store wraps this same type, so a bug here
// surfaces in face/voice biometrics and the routing KNN classifier
// alike.

func TestSet_RejectsEmptyInput(t *testing.T) {
	if err := New().Set(nil, nil); err == nil {
		t.Fatal("Set with no keys should fail")
	}
}

func TestSet_RejectsKeyValueLengthMismatch(t *testing.T) {
	err := New().Set(
		[][]float32{{1, 0, 0}},
		[][]byte{[]byte("a"), []byte("b")},
	)
	if err == nil {
		t.Fatal("len(keys) != len(values) should fail")
	}
}

func TestSet_RejectsDimensionMismatchOnLaterAdd(t *testing.T) {
	s := New()
	if err := s.Set([][]float32{{1, 0, 0}}, [][]byte{[]byte("3d")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set([][]float32{{1, 0}}, [][]byte{[]byte("2d")}); err == nil {
		t.Fatal("dimension mismatch on later Set should fail")
	}
}

func TestSet_RejectsDimensionMismatchWithinBatch(t *testing.T) {
	// A single Set whose batch contains mixed-dimension keys must
	// fail — silently accepting it would corrupt every later Find.
	err := New().Set(
		[][]float32{{1, 0, 0}, {1, 0}},
		[][]byte{[]byte("3d"), []byte("2d")},
	)
	if err == nil {
		t.Fatal("mixed-dimension within one batch should fail")
	}
}

func TestSet_MergesSortedAndUpdatesExistingKey(t *testing.T) {
	s := New()
	if err := s.Set([][]float32{{0.3, 0, 0}, {0.1, 0, 0}}, [][]byte{[]byte("c"), []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set([][]float32{{0.2, 0, 0}, {0.1, 0, 0}}, [][]byte{[]byte("b"), []byte("a-updated")}); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 {
		t.Fatalf("expected 3 unique keys, got %d", s.Len())
	}
	got, ok := singleGet(t, s, []float32{0.1, 0, 0})
	if !ok || string(got) != "a-updated" {
		t.Errorf("expected updated value, got %q", got)
	}
}

func TestGet_RoundTripsMultiKey(t *testing.T) {
	s := New()
	mustSet(t, s,
		[][]float32{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}, {0.7, 0.8, 0.9}},
		[][]byte{[]byte("a"), []byte("b"), []byte("c")},
	)
	keys, values, err := s.Get([][]float32{{0.7, 0.8, 0.9}, {0.1, 0.2, 0.3}})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || len(values) != 2 {
		t.Fatalf("expected 2 results, got %d / %d", len(keys), len(values))
	}
}

func TestGet_MissingKeyOmittedNotErrored(t *testing.T) {
	s := New()
	mustSet(t, s, [][]float32{{0.1, 0, 0}}, [][]byte{[]byte("a")})
	keys, _, err := s.Get([][]float32{{0.1, 0, 0}, {0.9, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 hit, got %d", len(keys))
	}
}

func TestDelete_RemovesAndPreservesSort(t *testing.T) {
	s := New()
	mustSet(t, s,
		[][]float32{{0.1, 0, 0}, {0.2, 0, 0}, {0.3, 0, 0}, {0.4, 0, 0}},
		[][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")},
	)
	if err := s.Delete([][]float32{{0.2, 0, 0}, {0.4, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 {
		t.Fatalf("expected 2 remaining, got %d", s.Len())
	}
}

func TestDelete_MissingKeyIsTolerated(t *testing.T) {
	s := New()
	mustSet(t, s, [][]float32{{0.1, 0, 0}}, [][]byte{[]byte("a")})
	if err := s.Delete([][]float32{{0.9, 0, 0}}); err != nil {
		t.Errorf("delete of missing key should succeed, got %v", err)
	}
	if s.Len() != 1 {
		t.Errorf("expected 1 surviving entry, got %d", s.Len())
	}
}

func TestFind_NormalizedTopK(t *testing.T) {
	s := New()
	mustSet(t, s,
		[][]float32{
			normalizeVec([]float32{1, 0, 0}),
			normalizeVec([]float32{0, 1, 0}),
			normalizeVec([]float32{0, 0, 1}),
		},
		[][]byte{[]byte("x"), []byte("y"), []byte("z")},
	)
	keys, values, sims, err := s.Find(normalizeVec([]float32{0.9, 0.1, 0}), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected top-2, got %d", len(keys))
	}
	if sims[0] < sims[1] {
		t.Errorf("results not sorted desc by similarity: %v", sims)
	}
	if string(values[0]) != "x" {
		t.Errorf("expected nearest = x, got %q", values[0])
	}
}

func TestFind_FallsBackForNonNormalizedKeys(t *testing.T) {
	s := New()
	mustSet(t, s,
		[][]float32{{2, 0, 0}, {0, 3, 0}},
		[][]byte{[]byte("x"), []byte("y")},
	)
	if s.HasNormalizedKeys() {
		t.Fatal("store should report non-normalized after Set with magnitude > 1")
	}
	_, values, sims, err := s.Find([]float32{4, 0, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(values[0]) != "x" {
		t.Errorf("expected x, got %q", values[0])
	}
	if !(sims[0] >= 0.99 && sims[0] <= 1.01) {
		t.Errorf("expected cos≈1.0 for parallel vectors, got %v", sims[0])
	}
}

func TestFind_RejectsZeroTopK(t *testing.T) {
	s := New()
	mustSet(t, s, [][]float32{{1, 0, 0}}, [][]byte{[]byte("x")})
	if _, _, _, err := s.Find([]float32{1, 0, 0}, 0); err == nil {
		t.Fatal("Find with topK=0 should fail")
	}
}

func TestFind_RejectsDimensionMismatch(t *testing.T) {
	s := New()
	mustSet(t, s, [][]float32{{1, 0, 0}}, [][]byte{[]byte("x")})
	if _, _, _, err := s.Find([]float32{1, 0}, 1); err == nil {
		t.Fatal("Find with mismatched dimension should fail")
	}
}

func TestFind_EmptyStoreReturnsEmptyResult(t *testing.T) {
	keys, _, _, err := New().Find([]float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("Find on empty store should succeed: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("empty store should return no results, got %d", len(keys))
	}
}

func TestFind_TopKLargerThanStore(t *testing.T) {
	// topK > store size must return all entries, not error.
	s := New()
	mustSet(t, s,
		[][]float32{normalizeVec([]float32{1, 0, 0}), normalizeVec([]float32{0, 1, 0})},
		[][]byte{[]byte("x"), []byte("y")},
	)
	keys, _, _, err := s.Find(normalizeVec([]float32{1, 0, 0}), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Errorf("expected all 2 entries, got %d", len(keys))
	}
}

// --- benchmarks ---

func BenchmarkFindNormalized(b *testing.B) {
	const dim = 768
	for _, n := range []int{8, 32, 128, 512} {
		b.Run(fmtN(n), func(b *testing.B) {
			s := buildStore(b, n, dim)
			query := normalizeVec(randVec(dim, 42))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, _, err := s.Find(query, 1); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// --- test helpers ---

func mustSet(t *testing.T, s *Store, keys [][]float32, values [][]byte) {
	t.Helper()
	if err := s.Set(keys, values); err != nil {
		t.Fatalf("Set: %v", err)
	}
}

func singleGet(t *testing.T, s *Store, key []float32) ([]byte, bool) {
	t.Helper()
	_, values, err := s.Get([][]float32{key})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(values) == 0 {
		return nil, false
	}
	return values[0], true
}

func buildStore(tb testing.TB, n, dim int) *Store {
	tb.Helper()
	s := New()
	keys := make([][]float32, n)
	values := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = normalizeVec(randVec(dim, int64(i)+1))
		values[i] = []byte{byte(i)}
	}
	if err := s.Set(keys, values); err != nil {
		tb.Fatal(err)
	}
	return s
}

func randVec(dim int, seed int64) []float32 {
	r := rand.New(rand.NewPCG(uint64(seed), 0xabcdef))
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

func normalizeVec(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	mag := math.Sqrt(sum)
	if mag == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / mag)
	}
	return out
}

func fmtN(n int) string {
	return map[int]string{8: "n=8", 32: "n=32", 128: "n=128", 512: "n=512"}[n]
}
