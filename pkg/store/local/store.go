// Package local is the in-process implementation of LocalAI's vector
// store. The gRPC backend at backend/go/local-store wraps this type;
// the routing module's KNN classifier embeds it directly. Callers
// must NOT build a parallel in-memory KNN index — extend this one.
//
// Storage is a sorted parallel-slice (keys [][]float32, values
// [][]byte). Set/Delete preserve the sort so Get can binary-search.
// Find scans linearly and uses a heap to keep the top-K — fine for
// the tens-to-thousands range. The "normalized fast path" (Find when
// every stored key has unit magnitude AND the query is normalized)
// skips the per-item magnitude calculation.
//
// Concurrency: the type is NOT safe for concurrent use. The gRPC
// backend serialises calls via base.SingleThread; in-process callers
// must wrap with a mutex if they share an instance across goroutines.
package local

import (
	"container/heap"
	"fmt"
	"math"
	"slices"
)

// Store is a key-value store keyed by float32 vectors. Optimized for
// nearest-neighbour search via Find.
type Store struct {
	keys   [][]float32
	values [][]byte

	// keysAreNormalized stays true until any non-unit-magnitude key
	// is added; once false, the magnitude-aware fallback path is
	// used by Find. Re-evaluated only at Set time, never again on
	// its own — a deletion of the offending key does NOT flip it
	// back to true (the bookkeeping cost would dominate the gain).
	keysAreNormalized bool

	// keyLen is the dimension of every stored key. -1 means "no
	// keys yet, dimension is open". Dimension mismatch on Set is
	// rejected so cosine similarity (which requires equal-length
	// vectors) doesn't silently mis-match.
	keyLen int
}

// New returns an empty Store with the dimension unset.
func New() *Store {
	return &Store{
		keys:              make([][]float32, 0),
		values:            make([][]byte, 0),
		keysAreNormalized: true,
		keyLen:            -1,
	}
}

// Len returns the number of stored entries.
func (s *Store) Len() int { return len(s.keys) }

// Dim returns the dimension of stored keys, or -1 when the store is empty.
func (s *Store) Dim() int { return s.keyLen }

// Set inserts or updates a batch of key/value pairs. Existing keys
// are replaced; new keys are merged into the sorted slice. The first
// Set fixes the dimension; later Sets at a different dimension fail.
func (s *Store) Set(keys [][]float32, values [][]byte) error {
	if len(keys) == 0 {
		return fmt.Errorf("local-store: Set: no keys to add")
	}
	if len(keys) != len(values) {
		return fmt.Errorf("local-store: Set: len(keys) = %d, len(values) = %d", len(keys), len(values))
	}

	if s.keyLen == -1 {
		s.keyLen = len(keys[0])
	} else if len(keys[0]) != s.keyLen {
		return fmt.Errorf("local-store: Set: key length %d does not match existing %d", len(keys[0]), s.keyLen)
	}

	kvs := make([]incomingPair, len(keys))
	for i, k := range keys {
		if len(k) != s.keyLen {
			return fmt.Errorf("local-store: Set: key %d length %d does not match existing %d", i, len(k), s.keyLen)
		}
		if s.keysAreNormalized && !isNormalized(k) {
			s.keysAreNormalized = false
		}
		kvs[i] = incomingPair{key: k, value: values[i]}
	}

	slices.SortFunc(kvs, func(a, b incomingPair) int { return slices.Compare(a.key, b.key) })

	merged := mergeSortedPairs(s.keys, s.values, kvs)
	s.keys = merged.keys
	s.values = merged.values
	return nil
}

// Get fetches values for the given keys. Missing keys are omitted
// from the result rather than reported as an error — callers compare
// returned-key length against requested-key length to detect them.
// Returned slices are aligned: foundKeys[i] corresponds to foundValues[i].
func (s *Store) Get(keys [][]float32) (foundKeys [][]float32, foundValues [][]byte, err error) {
	if len(s.keys) == 0 {
		return nil, nil, nil
	}
	if s.keyLen != -1 {
		for i, k := range keys {
			if len(k) != s.keyLen {
				return nil, nil, fmt.Errorf("local-store: Get: key %d length %d does not match existing %d", i, len(k), s.keyLen)
			}
		}
	}
	sortedKeys := append([][]float32(nil), keys...)
	slices.SortFunc(sortedKeys, slices.Compare[[]float32])

	tailK := s.keys
	tailV := s.values
	for _, k := range sortedKeys {
		j, ok := slices.BinarySearchFunc(tailK, k, slices.Compare[[]float32])
		if !ok {
			continue
		}
		foundKeys = append(foundKeys, tailK[j])
		foundValues = append(foundValues, tailV[j])
		tailK = tailK[j+1:]
		tailV = tailV[j+1:]
	}
	return foundKeys, foundValues, nil
}

// Delete removes the listed keys. Missing keys are tolerated — the
// shape callers (face/voice biometrics, future audit cleanup) often
// best-effort delete a batch and don't want to pre-check existence.
func (s *Store) Delete(keys [][]float32) error {
	if len(keys) == 0 {
		return fmt.Errorf("local-store: Delete: no keys to delete")
	}
	if s.keyLen != -1 {
		for i, k := range keys {
			if len(k) != s.keyLen {
				return fmt.Errorf("local-store: Delete: key %d length %d does not match existing %d", i, len(k), s.keyLen)
			}
		}
	}
	sortedKeys := append([][]float32(nil), keys...)
	slices.SortFunc(sortedKeys, slices.Compare[[]float32])

	mergedK := make([][]float32, 0, len(s.keys))
	mergedV := make([][]byte, 0, len(s.keys))
	tailK := s.keys
	tailV := s.values
	for _, k := range sortedKeys {
		j, ok := slices.BinarySearchFunc(tailK, k, slices.Compare[[]float32])
		if ok {
			mergedK = append(mergedK, tailK[:j]...)
			mergedV = append(mergedV, tailV[:j]...)
			tailK = tailK[j+1:]
			tailV = tailV[j+1:]
		}
	}
	mergedK = append(mergedK, tailK...)
	mergedV = append(mergedV, tailV...)
	s.keys = mergedK
	s.values = mergedV
	return nil
}

// Find returns the topK nearest stored entries by cosine similarity,
// ordered most-similar first. An empty store returns empty slices and
// no error — KNN routers that haven't loaded exemplars yet shouldn't
// have to special-case "warming up".
func (s *Store) Find(query []float32, topK int) (keys [][]float32, values [][]byte, similarities []float32, err error) {
	if topK < 1 {
		return nil, nil, nil, fmt.Errorf("local-store: Find: topK = %d, must be >= 1", topK)
	}
	if len(s.keys) == 0 {
		return nil, nil, nil, nil
	}
	if len(query) != s.keyLen {
		return nil, nil, nil, fmt.Errorf("local-store: Find: query length %d does not match existing %d", len(query), s.keyLen)
	}

	if s.keysAreNormalized && isNormalized(query) {
		k, v, sim := s.findNormalized(query, topK)
		return k, v, sim, nil
	}
	k, v, sim := s.findFallback(query, topK)
	return k, v, sim, nil
}

// HasNormalizedKeys reports whether every stored key currently passes
// the unit-magnitude check. Becomes false on the first non-normalized
// Set and stays false; a Delete does not restore it (see field comment).
func (s *Store) HasNormalizedKeys() bool { return s.keysAreNormalized }

func (s *Store) findNormalized(query []float32, topK int) (keys [][]float32, values [][]byte, similarities []float32) {
	pq := make(priorityQueue, 0, topK)
	heap.Init(&pq)
	for i, k := range s.keys {
		var dot float32
		for j := range k {
			dot += query[j] * k[j]
		}
		heap.Push(&pq, &priorityItem{similarity: dot, key: k, value: s.values[i]})
		if pq.Len() > topK {
			heap.Pop(&pq)
		}
	}
	return drainPQ(&pq)
}

func (s *Store) findFallback(query []float32, topK int) (keys [][]float32, values [][]byte, similarities []float32) {
	var qmag float64
	for _, v := range query {
		qmag += float64(v) * float64(v)
	}
	qmag = math.Sqrt(qmag)
	pq := make(priorityQueue, 0, topK)
	heap.Init(&pq)
	for i, k := range s.keys {
		var dot, kmag float64
		for j := range k {
			dot += float64(query[j]) * float64(k[j])
			kmag += float64(k[j]) * float64(k[j])
		}
		denom := qmag * math.Sqrt(kmag)
		var sim float32
		if denom > 0 {
			sim = float32(dot / denom)
		}
		heap.Push(&pq, &priorityItem{similarity: sim, key: k, value: s.values[i]})
		if pq.Len() > topK {
			heap.Pop(&pq)
		}
	}
	return drainPQ(&pq)
}

// --- helpers ---

func isNormalized(k []float32) bool {
	var sum float64
	for _, v := range k {
		sum += float64(v) * float64(v)
	}
	mag := math.Sqrt(sum)
	return mag >= 0.99 && mag <= 1.01
}

// IsNormalized reports whether v has unit magnitude (within 1%).
// Exposed so external callers (e.g. the routing KNN classifier) can
// avoid re-normalising vectors the embedder already returned at unit
// length.
func IsNormalized(v []float32) bool { return isNormalized(v) }

// Normalize returns a copy of v with unit magnitude, or v unchanged
// when its magnitude is already zero or close-to-one. Vectors fed
// to Find should match the dimension and magnitude of stored keys
// — normalising before insert and query keeps the fast path engaged.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	mag := math.Sqrt(sum)
	if mag == 0 || (mag >= 0.99 && mag <= 1.01) {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / mag)
	}
	return out
}

type incomingPair struct {
	key   []float32
	value []byte
}

type pairs struct {
	keys   [][]float32
	values [][]byte
}

// mergeSortedPairs merges (existing, incoming) into a fresh sorted
// slice. Equal keys take the incoming value — Set is upsert.
func mergeSortedPairs(existingK [][]float32, existingV [][]byte, incoming []incomingPair) pairs {
	l := len(existingK) + len(incoming)
	mk := make([][]float32, 0, l)
	mv := make([][]byte, 0, l)
	i, j := 0, 0
	for i < len(incoming) || j < len(existingK) {
		switch {
		case j >= len(existingK):
			mk = append(mk, incoming[i].key)
			mv = append(mv, incoming[i].value)
			i++
		case i >= len(incoming):
			mk = append(mk, existingK[j])
			mv = append(mv, existingV[j])
			j++
		default:
			c := slices.Compare(incoming[i].key, existingK[j])
			switch {
			case c < 0:
				mk = append(mk, incoming[i].key)
				mv = append(mv, incoming[i].value)
				i++
			case c > 0:
				mk = append(mk, existingK[j])
				mv = append(mv, existingV[j])
				j++
			default:
				// upsert: incoming wins
				mk = append(mk, incoming[i].key)
				mv = append(mv, incoming[i].value)
				i++
				j++
			}
		}
	}
	return pairs{keys: mk, values: mv}
}

// --- min-heap of (similarity, key, value), keeping the top-K ---

type priorityItem struct {
	similarity float32
	key        []float32
	value      []byte
}

type priorityQueue []*priorityItem

func (pq priorityQueue) Len() int            { return len(pq) }
func (pq priorityQueue) Less(i, j int) bool  { return pq[i].similarity < pq[j].similarity }
func (pq priorityQueue) Swap(i, j int)       { pq[i], pq[j] = pq[j], pq[i] }
func (pq *priorityQueue) Push(x any)         { *pq = append(*pq, x.(*priorityItem)) }
func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	*pq = old[0 : n-1]
	return item
}

// drainPQ pops the queue into descending-similarity order — the top
// item (most similar) ends up at index 0.
func drainPQ(pq *priorityQueue) (keys [][]float32, values [][]byte, similarities []float32) {
	n := pq.Len()
	keys = make([][]float32, n)
	values = make([][]byte, n)
	similarities = make([]float32, n)
	for i := n - 1; i >= 0; i-- {
		item := heap.Pop(pq).(*priorityItem)
		keys[i] = item.key
		values[i] = item.value
		similarities[i] = item.similarity
	}
	return keys, values, similarities
}
